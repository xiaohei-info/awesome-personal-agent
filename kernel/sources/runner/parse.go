package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// parsedEvent 是通过必填校验的标准事件（§9.2）。
type parsedEvent struct {
	ID              string    `json:"id"`
	Type            string    `json:"type"`
	Subject         string    `json:"subject"`
	Summary         string    `json:"summary,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	Link            string    `json:"link,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
	CorrelationKeys []string  `json:"correlation_keys,omitempty"`
	Sender          string    `json:"sender,omitempty"`
	SenderVerified  bool      `json:"sender_verified,omitempty"`
}

// Reject 是一条没有入账的输出行及原因（来源页可查）。
type Reject struct {
	SourceID string    `json:"source_id"`
	RunID    string    `json:"run_id"`
	Line     int       `json:"line"`
	Reason   string    `json:"reason"`
	Raw      string    `json:"raw"`
	At       time.Time `json:"at"`
}

type parsed struct {
	events   []parsedEvent
	rejects  []Reject // SourceID、RunID、At 由落账时补
	cursor   string
	cursorOK bool
}

const (
	rawLimit   = 512
	maxRejects = 1000 // 单次留痕的坏行数：输出上限是几十 MB，坏行不能换算成几千万条记录
)

// parse 解析 stdout：每行一个标准事件，末个非空行是 {"cursor":"…"}。
// 末行不是合法游标时 cursorOK 为假，其余事件照常返回。
func parse(stdout []byte) parsed {
	var p parsed
	lines := bytes.Split(stdout, []byte("\n"))
	type numbered struct {
		n    int
		text []byte
	}
	var ls []numbered
	for i, l := range lines {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			ls = append(ls, numbered{i + 1, l})
		}
	}
	if n := len(ls); n > 0 {
		if c, ok := asCursor(ls[n-1].text); ok {
			p.cursor, p.cursorOK = c, true
			ls = ls[:n-1]
		}
	}
	for _, l := range ls {
		ev, err := asEvent(l.text)
		if err != nil {
			if len(p.rejects) == maxRejects {
				continue
			}
			raw := string(l.text)
			if len(raw) > rawLimit {
				raw = raw[:rawLimit]
			}
			p.rejects = append(p.rejects, Reject{Line: l.n, Reason: err.Error(), Raw: raw})
			continue
		}
		p.events = append(p.events, ev)
	}
	return p
}

func asCursor(l []byte) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(l, &m) != nil || len(m) != 1 {
		return "", false
	}
	var c string
	if raw, ok := m["cursor"]; !ok || json.Unmarshal(raw, &c) != nil || c == "" {
		return "", false
	}
	return c, true
}

func asEvent(l []byte) (parsedEvent, error) {
	var ev parsedEvent
	if err := json.Unmarshal(l, &ev); err != nil {
		if _, ok := err.(*json.SyntaxError); ok {
			return ev, fmt.Errorf("not a JSON object")
		}
		return ev, fmt.Errorf("bad field: %v", err)
	}
	var missing []string
	for _, f := range []struct{ name, v string }{{"id", ev.ID}, {"type", ev.Type}, {"subject", ev.Subject}} {
		if strings.TrimSpace(f.v) == "" {
			missing = append(missing, f.name)
		}
	}
	if ev.OccurredAt.IsZero() {
		missing = append(missing, "occurred_at")
	}
	if len(missing) > 0 {
		return ev, fmt.Errorf("missing required field: %s", strings.Join(missing, ", "))
	}
	return ev, nil
}
