//go:build e2e

package channel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const srcID = "src_acc"

// accScript 是 ACC_SOURCE：输出队列目录里的事件，末行给游标（与 followmsg 的验收脚本同一做法）。
const accScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

// source 登记 ACC_SOURCE（每秒拉一次），之后用 emit 让它输出标准事件。
type source struct {
	f     *bg
	queue string
	seq   int
}

func (f *bg) newSource() *source {
	s := &source{f: f, queue: filepath.Join(f.Root, "acc-source")}
	must(f.t, os.MkdirAll(filepath.Join(s.queue, "in"), 0o755))
	must(f.t, os.MkdirAll(filepath.Join(s.queue, "done"), 0o755))
	script := filepath.Join(s.queue, "acc-source.sh")
	must(f.t, os.WriteFile(script, []byte(accScript), 0o755))
	body, _ := json.Marshal(map[string]any{"id": srcID, "type": "script", "source_type": "acc",
		"command": script + " " + s.queue, "schedule": "1s", "timeout": 30})
	f.webOK("POST", "/sources", string(body), 201)
	return s
}

// emit 让 ACC_SOURCE 在下一周期输出一条评论事件，关联键为 key；返回来源事件的去重 id。
func (s *source) emit(subject, sender string, verified bool, key string) string {
	s.seq++
	id := fmt.Sprintf("ev-%d", s.seq)
	ev := map[string]any{"id": id, "type": "comment.created", "subject": subject, "summary": "摘要 " + subject,
		"correlation_keys": []string{key}, "sender_verified": verified, "occurred_at": time.Now().UTC().Format(time.RFC3339)}
	if sender != "" {
		ev["sender"] = sender
	}
	b, _ := json.Marshal(ev)
	tmp := filepath.Join(s.queue, fmt.Sprintf("tmp-%d", s.seq))
	must(s.f.t, os.WriteFile(tmp, append(b, '\n'), 0o644))
	must(s.f.t, os.Rename(tmp, filepath.Join(s.queue, "in", fmt.Sprintf("%04d.jsonl", s.seq))))
	return id
}

type srcEvent struct {
	ID        string `json:"id"`
	DedupeKey struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	AttachState string `json:"attach_state"`
	Subject     string `json:"subject"`
}

// srcEvents 取来源页的事件（Web 看到的全部，不经任何过滤）。
func (s *source) events() []srcEvent {
	r := s.f.APIRaw("GET", "/v1/sources/"+srcID+"/events?limit=500", "", s.f.OBS)
	if r.Code != 200 {
		s.f.t.Fatalf("来源事件: %d %s", r.Code, r.Body)
	}
	var out struct {
		Events []srcEvent `json:"events"`
	}
	must(s.f.t, json.Unmarshal([]byte(r.Body), &out))
	return out.Events
}

func (s *source) event(id string) (srcEvent, bool) {
	for _, e := range s.events() {
		if e.DedupeKey.EventID == id {
			return e, true
		}
	}
	return srcEvent{}, false
}

// settled 等事件被拉取并过完流水线（disposition 不再是 pending）。
func (s *source) settled(id string) srcEvent {
	var ev srcEvent
	s.f.waitFor("事件 "+id+" 完成处置", 30*time.Second, func() bool {
		var ok bool
		ev, ok = s.event(id)
		return ok && ev.Disposition != "pending"
	})
	return ev
}
