package imports

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
)

// 单次请求的上限（§7.1）：超过由传输层返回 413。
const (
	MaxRecords = 500
	MaxBytes   = 8 << 20
)

// maxHistory 是单条事项历史的条数上限；maxExternalID 限制外部 id 的长度。
const (
	maxHistory    = 500
	maxExternalID = 256
)

var sourceName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidSource 报告 s 是不是合法的来源名：小写字母、数字、连字符，不超过 32 个字符（§7.1）。
func ValidSource(s string) bool { return sourceName.MatchString(s) }

// lineError 是一行记录不合法：Rule 是稳定的规则名（响应里的 code），why 说明哪里不合法。
// 它包着一个 invalid_request 的领域错误，所以管线之外的调用方也能按退出码 2 处理。
type lineError struct {
	Rule string
	err  *domain.Error
}

func (e *lineError) Error() string { return e.Rule + ": " + e.err.Why }
func (e *lineError) Unwrap() error { return e.err }

func bad(rule, format string, a ...any) *lineError {
	return &lineError{Rule: rule, err: domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(format, a...), "fix the record and retry; invalid records are skipped")}
}

// asLine 把任何错误还原为行错误：不是行错误但是 invalid_request 的（管线的内核字段防写、事项自身校验）也算这一行不合法。
func asLine(err error) (*lineError, bool) {
	var le *lineError
	if errors.As(err, &le) {
		return le, true
	}
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeInvalidRequest {
		return &lineError{Rule: "invalid_record", err: de}, true
	}
	return nil, false
}

// ---- 记录的 JSON 形状（§7.1） ----

type head struct {
	Type       string `json:"type"`
	ExternalID string `json:"external_id"`
}

type projectRec struct {
	head
	Name      string               `json:"name"`
	Intro     string               `json:"intro"`
	Tags      []string             `json:"tags"`
	Status    domain.ProjectStatus `json:"status"`
	CreatedAt string               `json:"created_at"`
}

type itemBody struct {
	Kind        domain.Kind       `json:"kind"`
	Title       string            `json:"title"`
	Summary     string            `json:"summary"`
	Body        string            `json:"body"`
	StatusLabel string            `json:"status_label"`
	Priority    domain.Priority   `json:"priority"`
	Tags        []string          `json:"tags"`
	WaitingOn   *domain.WaitingOn `json:"waiting_on"`
	Deadline    *time.Time        `json:"deadline"`
	PlannedFor  string            `json:"planned_for"`
	Spec        json.RawMessage   `json:"spec"`
	Ext         json.RawMessage   `json:"ext"`
	Assignee    json.RawMessage   `json:"assignee"`
}

type historyRec struct {
	At        string       `json:"at"`
	Event     string       `json:"event"`
	Phase     domain.Phase `json:"phase"` // created
	To        domain.Phase `json:"to"`    // transition
	Reason    string       `json:"reason"`
	Text      string       `json:"text"` // progress
	On        string       `json:"on"`
	Precision string       `json:"precision"`
}

type itemRec struct {
	head
	Project string       `json:"project"`
	Item    itemBody     `json:"item"`
	History []historyRec `json:"history"`
}

// parsed 是一行解析并做完不依赖账本的校验后的结果。
type parsed struct {
	Type       string // project、item
	ExternalID string
	Project    projects.ImportedProject
	// 事项记录：ProjectRef 是 project 字段（同来源名的项目外部 id 或已有项目 id，可空）。
	ProjectRef string
	Item       items.ImportedItem
}

func strict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err == nil {
		return errors.New("trailing content")
	}
	return nil
}

func parseTime(rule, field, s string) (time.Time, *lineError) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, bad(rule, "%s must be an RFC 3339 time", field)
	}
	return t, nil
}

// parseLine 解析并校验一行（不查账本）。now 是当前时刻：历史里的时刻不得晚于它。source 用来定 origin。
func parseLine(raw []byte, source string, now time.Time) (*parsed, *lineError) {
	var h head
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return nil, bad("bad_json", "the line is not a JSON object")
	}
	_ = json.Unmarshal(probe["type"], &h.Type)
	_ = json.Unmarshal(probe["external_id"], &h.ExternalID)
	switch {
	case h.Type != "project" && h.Type != "item":
		return nil, bad("bad_type", "type must be project or item")
	case strings.TrimSpace(h.ExternalID) == "" || len(h.ExternalID) > maxExternalID:
		return nil, bad("bad_external_id", "external_id is required and at most %d characters", maxExternalID)
	}
	p := &parsed{Type: h.Type, ExternalID: h.ExternalID}
	if h.Type == "project" {
		var r projectRec
		if err := strict(raw, &r); err != nil {
			return nil, bad("bad_record", "project record: %v", err)
		}
		p.Project = projects.ImportedProject{Name: r.Name, Intro: r.Intro, Tags: r.Tags, Status: r.Status, CreatedAt: now}
		if r.CreatedAt != "" {
			t, le := parseTime("bad_created_at", "created_at", r.CreatedAt)
			if le != nil {
				return nil, le
			}
			if t.After(now) {
				return nil, bad("history_future", "created_at is later than now")
			}
			p.Project.CreatedAt = t
		}
		if strings.TrimSpace(r.Name) == "" {
			return nil, bad("name_missing", "project name is required")
		}
		if r.Status != "" && r.Status != domain.ProjectActive && r.Status != domain.ProjectPaused && r.Status != domain.ProjectArchived {
			return nil, bad("bad_status", "status must be active, paused or archived")
		}
		if slices.ContainsFunc(r.Tags, func(t string) bool { return strings.TrimSpace(t) == "" }) {
			return nil, bad("bad_tags", "tags must not be blank")
		}
		return p, nil
	}
	var r itemRec
	if err := strict(raw, &r); err != nil {
		return nil, bad("bad_record", "item record: %v", err)
	}
	im, le := buildItem(r, source, now)
	if le != nil {
		return nil, le
	}
	p.ProjectRef, p.Item = r.Project, im
	return p, nil
}

func buildItem(r itemRec, source string, now time.Time) (items.ImportedItem, *lineError) {
	var im items.ImportedItem
	b := r.Item
	switch {
	case b.Kind == domain.KindDelegation || b.Kind == domain.KindBrief:
		return im, bad("kind_not_allowed", "kind %s cannot be imported", b.Kind)
	case len(bytes.TrimSpace(b.Assignee)) > 0:
		return im, bad("assignee_not_allowed", "assignee cannot be given: imported items belong to the owner")
	case len(r.History) == 0 || r.History[0].Event != items.StepCreated:
		return im, bad("history_first", "the first history entry must be created")
	case len(r.History) > maxHistory:
		return im, bad("history_too_long", "history has more than %d entries", maxHistory)
	}
	type at struct {
		t time.Time
		h historyRec
	}
	ents := make([]at, len(r.History))
	for i, h := range r.History {
		t, le := parseTime("history_at", fmt.Sprintf("history[%d].at", i), h.At)
		if le != nil {
			return im, le
		}
		if t.After(now) {
			return im, bad("history_future", "history[%d].at is later than now", i)
		}
		if h.Precision != "" && h.Precision != "day" {
			return im, bad("bad_precision", "history[%d].precision must be day or empty", i)
		}
		ents[i] = at{t, h}
	}
	slices.SortStableFunc(ents, func(a, c at) int { return a.t.Compare(c.t) })
	if ents[0].h.Event != items.StepCreated {
		return im, bad("history_first", "created must be the earliest history entry")
	}
	final := domain.Item{Kind: b.Kind, Title: b.Title, Summary: b.Summary, Body: b.Body, StatusLabel: b.StatusLabel, Priority: b.Priority,
		Tags: b.Tags, WaitingOn: b.WaitingOn, Deadline: b.Deadline, PlannedFor: b.PlannedFor, Spec: b.Spec, Ext: b.Ext,
		Origin: &domain.Origin{Source: "import:" + source, Ref: r.ExternalID}}
	if final.Priority == "" {
		final.Priority = domain.PriorityNormal
	}
	phase := func(i int, p domain.Phase) *lineError {
		switch {
		case !p.Valid():
			return bad("bad_phase", "history[%d]: unknown phase %q", i, p)
		case p == domain.PhaseNeedsInput:
			return bad("needs_input_not_allowed", "history[%d]: needs_input is a pending decision, not history; ask after the import", i)
		case p == domain.PhaseProposed:
			return bad("bad_phase", "history[%d]: proposed is the suggestion stream, not a state of imported history", i)
		case p == domain.PhaseWaiting && b.WaitingOn == nil:
			return bad("waiting_on_missing", "history[%d]: waiting needs item.waiting_on", i)
		}
		return nil
	}
	var steps []items.ImportStep
	for i, e := range ents {
		h := e.h
		s := items.ImportStep{At: e.t, Event: h.Event, Precision: h.Precision}
		switch h.Event {
		case items.StepCreated:
			if i != 0 {
				return im, bad("history_created_twice", "only the first history entry can be created")
			}
			if le := phase(i, h.Phase); le != nil {
				return im, le
			}
			s.Phase, s.Reason = h.Phase, "created"
		case items.StepTransition:
			if le := phase(i, h.To); le != nil {
				return im, le
			}
			if h.To == domain.PhaseDropped && strings.TrimSpace(h.Reason) == "" {
				return im, bad("reason_missing", "history[%d]: a transition to dropped needs a reason", i)
			}
			s.Phase, s.Reason = h.To, h.Reason
			if strings.TrimSpace(s.Reason) == "" {
				s.Reason = "imported"
			}
		case items.StepProgress:
			if strings.TrimSpace(h.Text) == "" {
				return im, bad("progress_text", "history[%d]: progress needs text", i)
			}
			if _, err := time.Parse("2006-01-02", h.On); err != nil || h.On == "" {
				return im, bad("progress_on", "history[%d].on must be a date in YYYY-MM-DD form", i)
			}
			if h.On > e.t.Format("2006-01-02") { // 按 at 自带的时区取"那一天"
				return im, bad("progress_on_late", "history[%d].on is later than the day of at", i)
			}
			s.Text, s.On = h.Text, h.On
		default:
			return im, bad("bad_event", "history[%d].event must be created, transition or progress", i)
		}
		steps = append(steps, s)
	}
	final.Phase = steps[0].Phase
	for _, s := range steps {
		if s.Event == items.StepTransition {
			final.Phase = s.Phase
		}
	}
	return items.ImportedItem{Item: final, Steps: steps}, nil
}
