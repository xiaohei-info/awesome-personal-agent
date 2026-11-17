// Package imports 是带原始时间的导入（设计 §7.1）：import.apply 命令、批次与对照、逐行校验与预检。
//
// 一次导入是若干次 import.apply：每条记录一次命令、一个事务（连同它的全部历史要么都在要么都不在），
// 批次内各条各自成败，中途失败时已写入的留着、重跑按（来源名，外部 id）对照幂等地从断点继续。
// 一个请求的最后再以同一命令记一条 import.completed。事项与项目的写入、批次与对照表归各自的模块（items、projects），
// 本包只做解析、校验与编排；它不认识任何具体的来源工具。
package imports

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Command 是导入命令的名字（§14.5：只接受授权单）。
const Command = policy.ImportApply

// 单条记录的结果。
const (
	OutcomeCreated  = "created"
	OutcomeExisting = "existing"
	OutcomeInvalid  = "invalid"
)

// Options 配置导入。
type Options struct {
	// Kinds 用来按 kind 的 spec schema 校验事项；nil 表示没有登记任何 kind schema。
	Kinds *items.Kinds
	// Waiting 在导入的 waiting 事项写入的同一事务里被调用，为它从导入时刻起排第一轮跟进（§7.1）；nil 不排。
	Waiting func(tx *store.Tx, it domain.Item, now time.Time) error
	// Now 是 dry_run 与校验用的当前时刻，须与命令执行器用同一个时钟；默认 clock.Now。
	Now func() time.Time
}

// Service 是导入的入口：POST /import、GET /imports 与 apa import 都经它。
type Service struct {
	x *command.Executor
	o Options
}

// Register 在命令执行器上登记 import.apply 并返回服务。
func Register(x *command.Executor, o Options) (*Service, error) {
	if o.Now == nil {
		o.Now = clock.Now
	}
	s := &Service{x: x, o: o}
	return s, x.Register(s.spec())
}

// applyIn 是 import.apply 的输入：Record 与 Completed 二选一——一条记录，或一个请求写完的收尾。
type applyIn struct {
	Source    string          `json:"source"`
	Batch     string          `json:"batch"`
	Record    json.RawMessage `json:"record,omitempty"`
	Completed *Counts         `json:"completed,omitempty"`
}

// Counts 是一个请求里三种结果的条数。
type Counts struct {
	Created  int `json:"created"`
	Existing int `json:"existing"`
	Invalid  int `json:"invalid"`
}

// applied 是 import.apply 处理一条记录的结果。
type applied struct {
	Outcome string `json:"outcome"`
	Object  string `json:"object,omitempty"`
}

func decode(in json.RawMessage) (applyIn, error) {
	var v applyIn
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
	}
	return v, nil
}

func invalidReq(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func (s *Service) spec() command.Spec {
	return command.Spec{Name: Command, Action: authz.ActionEditAnyItem, Tier: authz.TierDecide,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			_, err := decode(in)
			return authz.Object{}, err
		},
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, err := decode(in)
			switch {
			case err != nil:
				return err
			case !ValidSource(v.Source):
				return invalidReq("source must be 1-32 characters of lowercase letters, digits and hyphens")
			case (len(v.Record) == 0) == (v.Completed == nil):
				return invalidReq("give either a record or completed")
			case v.Completed != nil && (v.Completed.Created < 0 || v.Completed.Existing < 0 || v.Completed.Invalid < 0):
				return invalidReq("completed counts cannot be negative")
			}
			return nil
		},
		Apply: s.apply}
}

func (s *Service) apply(c *command.Ctx) (any, error) {
	v, _ := decode(c.Input)
	now := c.Now()
	b, err := items.ImportBatchEnsure(c.Tx, items.ImportBatch{ID: v.Batch, Source: v.Source, Actor: c.Caller.Actor.Principal, Grant: c.Grant, At: now})
	if err != nil {
		return nil, err
	}
	if b.Source != v.Source || b.Actor != c.Caller.Actor.Principal {
		return nil, domain.NewError(domain.CodePermissionDenied, "the batch does not belong to this source name and caller", "start a new batch")
	}
	if v.Completed != nil {
		pl, _ := json.Marshal(map[string]any{"source": v.Source, "created": v.Completed.Created, "existing": v.Completed.Existing, "invalid": v.Completed.Invalid})
		_, err := c.Emit(domain.Event{Type: domain.EvImportCompleted, Object: v.Batch, Payload: pl, At: now})
		return applied{Outcome: "completed"}, err
	}
	p, le := parseLine(v.Record, v.Source, now)
	if le != nil {
		return nil, le
	}
	plan, err := s.prepare(&resolver{tx: c.Tx, source: v.Source}, p)
	switch {
	case err != nil:
		return nil, err
	case plan.existing && !plan.bind:
		return applied{OutcomeExisting, plan.object}, nil
	case plan.bind: // 与已有同名项目绑定：不改它，只登记对照；不写事件、不占条数
		return applied{OutcomeExisting, plan.object}, items.ImportMapPut(c.Tx, v.Source, p.Type, p.ExternalID, plan.object)
	}
	if c.Grant != "" { // 凭单导入：条数与来源名在写事务里原子扣减，失败回滚时一并撤销
		if err := policy.ConsumeImport(c.Tx, c.Grant, v.Source, 1, now); err != nil {
			return nil, err
		}
	}
	info := domain.ImportInfo{Batch: v.Batch, At: now}
	var id string
	if p.Type == "project" {
		id, err = projects.ImportCreate(c, p.Project, v.Batch, now)
	} else {
		p.Item.Item.Project = plan.project
		if id, err = items.WriteImported(c, p.Item, info); err == nil && s.o.Waiting != nil && p.Item.Item.Phase == domain.PhaseWaiting {
			var it domain.Item
			if it, err = items.Get(c.Tx, id); err == nil {
				err = s.o.Waiting(c.Tx, it, now)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err := items.ImportMapPut(c.Tx, v.Source, p.Type, p.ExternalID, id); err != nil {
		return nil, err
	}
	if err := items.ImportBatchAdd(c.Tx, v.Batch, 1); err != nil {
		return nil, err
	}
	return applied{OutcomeCreated, id}, nil
}

// resolver 在一个事务里解析记录对账本的引用。pending 只在预检里用：同一文件里前面的项目记录"会被建出来"，
// 但预检不写账本，后面的事项引用它时按已存在处理。
type resolver struct {
	tx      *store.Tx
	source  string
	pending map[string]bool // 预检：本请求里前面项目记录的外部 id
	seen    map[string]bool // 预检：本请求里已出现的（类型, 外部 id）
}

// planned 是一条记录对当前账本状态的判定。
type planned struct {
	existing bool   // 对照已有（或本请求里前面出现过）：整条跳过
	bind     bool   // 项目记录：没有对照但已有同名项目，绑定过去（不写事件）
	object   string // existing 与 bind 时的账本对象 id
	project  string // 事项：解析出的项目 id；预检里引用的是本请求前面的项目时为空
}

// project 解析事项记录的 project 字段：同来源名下项目记录的外部 id，或已有项目的 id；空表示不属于任何项目。
// pending 为真表示它是预检里本请求前面的项目记录，还没有账本 id。
func (r *resolver) project(ref string) (id string, pending bool, err error) {
	if ref == "" {
		return "", false, nil
	}
	if obj, ok, err := items.ImportMapGet(r.tx, r.source, "project", ref); err != nil || ok {
		return obj, false, err
	}
	if r.pending[ref] {
		return "", true, nil
	}
	if _, err := projects.Get(r.tx, ref); err == nil {
		return ref, false, nil
	} else if de := (*domain.Error)(nil); !errors.As(err, &de) || de.Code != domain.CodeNotFound {
		return "", false, err
	}
	return "", false, bad("project_unknown", "project %q is neither a project record of this source nor an existing project", ref)
}

// prepare 做依赖账本的校验并给出判定：幂等对照、项目绑定、项目引用与事项自身校验（同普通写入）。
func (s *Service) prepare(r *resolver, p *parsed) (planned, error) {
	var pl planned
	key := p.Type + "\x00" + p.ExternalID
	if obj, ok, err := items.ImportMapGet(r.tx, r.source, p.Type, p.ExternalID); err != nil {
		return pl, err
	} else if ok {
		pl.existing, pl.object = true, obj
		return pl, nil
	}
	if r.seen[key] {
		pl.existing = true
		return pl, nil
	}
	if r.seen != nil {
		r.seen[key] = true
	}
	if p.Type == "project" {
		if r.pending != nil {
			r.pending[p.ExternalID] = true
		}
		if found, ok, err := projects.FindByName(r.tx, p.Project.Name); err != nil {
			return pl, err
		} else if ok {
			pl.existing, pl.bind, pl.object = true, true, found.ID
		}
		return pl, nil
	}
	proj, pending, err := r.project(p.ProjectRef)
	if err != nil {
		return pl, err
	}
	pl.project = proj
	// 阶段快照逐个过普通写入的校验；预检里引用的项目还不存在时先按没有项目校验。
	kinds := s.o.Kinds
	for _, st := range []domain.Phase{p.Item.Steps[0].Phase, p.Item.Item.Phase} {
		it := p.Item.Item
		it.Phase, it.Project = st, proj
		if st != domain.PhaseWaiting {
			it.WaitingOn = nil
		}
		if pending {
			it.Project = ""
		}
		if err := kinds.CheckImported(r.tx, it); err != nil {
			return pl, err
		}
	}
	return pl, nil
}

// ---- 请求 ----

// Params 是一次导入请求。
type Params struct {
	Identity command.Identity
	Source   string
	Batch    string // 空表示新开一个批次
	DryRun   bool
	Lines    []Line
	IdemKey  string
	Evidence string
	Grant    string
	WebURL   string
}

// Line 是请求体里的一行非空记录；N 是它在请求体里的行号（从 1 起）。
type Line struct {
	N    int
	Text []byte
}

// LineResult 是一行的结果。
type LineResult struct {
	Line       int    `json:"line"`
	ExternalID string `json:"external_id,omitempty"`
	Outcome    string `json:"outcome"`
	Object     string `json:"object,omitempty"`
	Code       string `json:"code,omitempty"`
	Why        string `json:"why,omitempty"`
}

// Result 是一次导入请求的结果；预检没有 Batch。
type Result struct {
	Batch   string       `json:"batch,omitempty"`
	Results []LineResult `json:"results"`
	Counts
}

func (c *Counts) add(o string) {
	switch o {
	case OutcomeCreated:
		c.Created++
	case OutcomeExisting:
		c.Existing++
	default:
		c.Invalid++
	}
}

func externalID(raw []byte) string {
	var h head
	_ = json.Unmarshal(raw, &h)
	return h.ExternalID
}

// Run 执行一次导入请求。先过授权（预检同样），再逐行判定；真实导入逐条记录一个事务地写入，最后记 import.completed。
func (s *Service) Run(ctx context.Context, p Params) (*Result, error) {
	if !ValidSource(p.Source) {
		return nil, invalidReq("source must be 1-32 characters of lowercase letters, digits and hyphens")
	}
	req := func(in applyIn) command.Request {
		b, _ := json.Marshal(in)
		return command.Request{Identity: p.Identity, Command: Command, Input: b, Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL}
	}
	// 授权先于一切：没有授权单、来源名不符或条数超限都在写任何东西之前返回。
	pre := req(applyIn{Source: p.Source, Completed: &Counts{}})
	pre.Preflight = true
	if _, err := s.x.Do(ctx, pre); err != nil {
		return nil, err
	}
	res := &Result{Results: make([]LineResult, 0, len(p.Lines))}
	outcomes := make([]LineResult, len(p.Lines))
	skip := make([]bool, len(p.Lines)) // 真实导入里不必再走命令的行：不合法的与已有对照的
	var principal string
	var created int
	now := s.o.Now()
	err := s.x.Store().View(ctx, func(tx *store.Tx) error {
		caller, err := p.Identity.Authenticate(tx)
		if err != nil {
			return err
		}
		principal = caller.Actor.Principal
		if p.Batch != "" {
			b, ok, err := items.ImportBatchGet(tx, p.Batch)
			if err != nil {
				return err
			}
			if !ok || b.Source != p.Source || b.Actor != principal {
				return domain.NewError(domain.CodeInvalidRequest, "the batch does not belong to this source name and caller", "use the batch returned by an earlier request of the same source, or omit it")
			}
		}
		r := &resolver{tx: tx, source: p.Source, pending: map[string]bool{}, seen: map[string]bool{}}
		for i, ln := range p.Lines {
			lr := LineResult{Line: ln.N, ExternalID: externalID(ln.Text)}
			parsedLine, le := parseLine(ln.Text, p.Source, now)
			var pl planned
			var err error
			if le == nil {
				pl, err = s.prepare(r, parsedLine)
				if err != nil {
					if le, _ = asLine(err); le == nil {
						return err
					}
				}
			}
			switch {
			case le != nil:
				lr.Outcome, lr.Code, lr.Why = OutcomeInvalid, le.Rule, le.err.Why
				skip[i] = le.Rule == "bad_json" // 其余不合法的行仍交给命令：写入时按同一规则再判一次，以账本为准
			case pl.existing:
				lr.Outcome, lr.Object = OutcomeExisting, pl.object
				skip[i] = !pl.bind && pl.object != "" && p.IdemKey == "" // 带幂等键时仍走命令，重放才得到首次的结果
			default:
				lr.Outcome = OutcomeCreated
				created++
			}
			outcomes[i] = lr
		}
		if caller.Level == authz.LevelDelegate && p.Grant != "" {
			return policy.CheckImport(tx, p.Grant, p.Source, created, now)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if p.DryRun {
		for _, lr := range outcomes {
			res.Results = append(res.Results, lr)
			res.add(lr.Outcome)
		}
		return res, nil
	}
	res.Batch = p.Batch
	if res.Batch == "" {
		if res.Batch, err = newBatchID(principal, p.Source, p.IdemKey); err != nil {
			return nil, err
		}
	}
	for i, ln := range p.Lines {
		lr := outcomes[i]
		if !skip[i] {
			in := req(applyIn{Source: p.Source, Batch: res.Batch, Record: json.RawMessage(ln.Text)})
			if p.IdemKey != "" {
				in.IdemKey = p.IdemKey + ":" + strconv.Itoa(ln.N)
			}
			out, err := s.x.Do(ctx, in)
			if err != nil {
				le, ok := asLine(err)
				if !ok {
					return nil, err
				}
				lr = LineResult{Line: ln.N, ExternalID: lr.ExternalID, Outcome: OutcomeInvalid, Code: le.Rule, Why: le.err.Why}
			} else {
				var a applied
				if err := json.Unmarshal(out.Value, &a); err != nil {
					return nil, err
				}
				lr = LineResult{Line: ln.N, ExternalID: lr.ExternalID, Outcome: a.Outcome, Object: a.Object}
			}
		}
		res.Results = append(res.Results, lr)
		res.add(lr.Outcome)
	}
	done := req(applyIn{Source: p.Source, Batch: res.Batch, Completed: &res.Counts})
	if p.IdemKey != "" {
		done.IdemKey = p.IdemKey + ":done"
	}
	if _, err := s.x.Do(ctx, done); err != nil {
		return nil, err
	}
	return res, nil
}

// newBatchID：带幂等键的请求重放时要得到同一个批次号，所以由（调用者，来源名，键）定；否则随机。
func newBatchID(principal, source, idemKey string) (string, error) {
	if idemKey != "" {
		h := sha256.Sum256([]byte(principal + "\x00" + source + "\x00" + idemKey))
		return "imp_" + hex.EncodeToString(h[:8]), nil
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "imp_" + hex.EncodeToString(b), nil
}

// ---- 批次列表 ----

// Batch 是 GET /imports 的一项。
type Batch struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Created   int       `json:"created"`
	Existing  int       `json:"existing"`
	Invalid   int       `json:"invalid"`
	StartedAt time.Time `json:"started_at"`
	Actor     string    `json:"actor"`
	Grant     string    `json:"grant,omitempty"`
}

// Batches 返回全部导入批次，最新在前。已新建的条数取批次表（写入即计，中途失败也准）；已存在与不合法的条数是各次请求的
// import.completed 之和。
func Batches(tx *store.Tx) ([]Batch, error) {
	bs, err := items.ImportBatches(tx)
	if err != nil {
		return nil, err
	}
	out := make([]Batch, 0, len(bs))
	for _, b := range bs {
		x := Batch{ID: b.ID, Source: b.Source, Created: b.Count, StartedAt: b.At, Actor: b.Actor, Grant: b.Grant}
		es, err := eventlog.Query(tx, eventlog.Filter{Object: b.ID, Type: domain.EvImportCompleted}, 0, 100000)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			var c Counts
			if err := json.Unmarshal(e.Event.Payload, &c); err != nil {
				return nil, fmt.Errorf("imports: %s: corrupt import.completed: %w", b.ID, err)
			}
			x.Existing += c.Existing
			x.Invalid += c.Invalid
		}
		out = append(out, x)
	}
	return out, nil
}
