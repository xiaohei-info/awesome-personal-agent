//go:build e2e

package automation

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const accTagPack = `name: acc-rule
version: 1.0.0
description: 规则模式验收
event_rules:
  - id: acc-tag
    mode: observe
    when:
      event: item.phase_changed
      to_phase: open
      tag: [acc-rule]
    do:
      - action: add_tag
        tag: acc-hit
`

const accLoopPack = `name: acc-loop
version: 1.0.0
description: 规则不自触发
event_rules:
  - id: loop-a
    when:
      tag: acc-loop
    do:
      - action: set_phase
        phase: waiting
  - id: loop-b
    when:
      event: item.phase_changed
    do:
      - action: add_tag
        tag: acc-b
`

type item struct {
	ID    string   `json:"id"`
	Phase string   `json:"phase"`
	Tags  []string `json:"tags"`
}

func (i item) has(tag string) bool {
	for _, t := range i.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func TestFlowRuleModes(t *testing.T) {
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	packsDir := filepath.Join(e.Home, "packs")

	newItem := func(tag string) string {
		t.Helper()
		r := e.Apa(e.DEL, "", "item", "add", "--kind", "task", "--title", "rule-"+tag, "--tags", tag, "--reason", "rule e2e", "--evidence", "rule e2e")
		var it item
		if r.Code != 0 {
			t.Fatalf("建事项: %d %s", r.Code, r.Stderr)
		}
		jsonOf(t, r.Stdout, &it)
		return it.ID
	}
	open := func(id string) {
		t.Helper()
		if r := e.Apa(e.DEL, "", "item", "transition", id, "--to", "open", "--reason", "rule e2e", "--evidence", "rule e2e"); r.Code != 0 {
			t.Fatalf("事项 %s 转 open: %d %s", id, r.Code, r.Stderr)
		}
	}
	show := func(id string) item {
		t.Helper()
		r := e.Apa(e.OBS, "", "item", "show", id)
		var out struct {
			Item item `json:"item"`
		}
		if r.Code != 0 {
			t.Fatalf("item show: %d %s", r.Code, r.Stderr)
		}
		jsonOf(t, r.Stdout, &out)
		return out.Item
	}
	why := func(id string) string {
		t.Helper()
		r := e.Apa(e.DEL, "", "why", id)
		if r.Code != 0 {
			t.Fatalf("apa why: %d %s", r.Code, r.Stderr)
		}
		return r.Stdout
	}
	// 规则键含 "/"，放进路径要转义。
	modePath := func(key string) string { return "/v1/rules/" + url.PathEscape(key) + "/mode" }
	const tagKey = "acc-rule/event_rules/acc-tag"

	step(t, "ACT-RULE-01 准备规则", func(t *testing.T) {
		writePack(t, packsDir, "acc-rule", map[string]string{"pack.yaml": accTagPack})
		reload(e)
		if s, ok := rules(t, e)["acc-tag"]; !ok || s.Mode != "observe" || s.Key != tagKey {
			t.Fatalf("GET /v1/rules 应显示 acc-tag 为 observe: %+v", s)
		}
	})

	step(t, "ACT-RULE-02 observe 只记录本来会做什么", func(t *testing.T) {
		id := newItem("acc-rule")
		open(id)
		w := waitWhy(t, why, id, "rule.observed")
		if !contains(w, tagKey) {
			t.Errorf("apa why 应有\"本来会做什么\"的记录: %s", w)
		}
		if show(id).has("acc-hit") {
			t.Error("observe 不得改业务状态")
		}
		if strings.Contains(w, "outcall") {
			t.Errorf("observe 不得外呼: %s", w)
		}
	})

	step(t, "ACT-RULE-03 只在 Web 切换到 auto", func(t *testing.T) {
		r := e.APIRaw("PATCH", modePath(tagKey), `{"mode":"auto"}`, e.DEL)
		if r.Code != 403 {
			t.Errorf("token 切 auto 应 403，实际 %d %s", r.Code, r.Body)
		}
		if s := rules(t, e)["acc-tag"]; s.Mode != "observe" {
			t.Errorf("被拒后模式应不变: %+v", s)
		}
		if r := e.Web("PATCH", modePath(tagKey), `{"mode":"auto"}`); r.Code != 200 {
			t.Fatalf("Web 切 auto: %d %s", r.Code, r.Body)
		}
		if s := rules(t, e)["acc-tag"]; s.Mode != "auto" {
			t.Errorf("Web 切换后应为 auto: %+v", s)
		}
		ev := e.Apa(e.OBS, "", "events", "--limit", "500")
		if !contains(ev.Stdout, "rule.mode_changed", tagKey) {
			t.Errorf("事件流应有 rule.mode_changed: %s", ev.Stdout)
		}
	})

	step(t, "ACT-RULE-04 auto 时执行动作并记授权依据", func(t *testing.T) {
		id := newItem("acc-rule")
		open(id)
		waitUntil(t, "auto 时事项应有 acc-hit", func() bool { return show(id).has("acc-hit") })
		var tl struct {
			Steps []struct {
				Type  string `json:"type"`
				Basis *struct {
					PolicyVersion *int   `json:"policy_version"`
					RuleID        string `json:"rule_id"`
				} `json:"basis"`
			} `json:"steps"`
		}
		jsonOf(t, why(id), &tl)
		found := false
		for _, s := range tl.Steps {
			if s.Basis != nil && s.Basis.RuleID == tagKey && s.Basis.PolicyVersion != nil {
				found = true
			}
		}
		if !found {
			t.Errorf("Event 应记 basis（策略版本与规则 id）: %+v", tl.Steps)
		}
	})

	step(t, "ACT-RULE-05 off 时跳过", func(t *testing.T) {
		if r := e.Web("PATCH", modePath(tagKey), `{"mode":"off"}`); r.Code != 200 {
			t.Fatalf("Web 切 off: %d %s", r.Code, r.Body)
		}
		id := newItem("acc-rule")
		open(id)
		time.Sleep(settle)
		if show(id).has("acc-hit") {
			t.Error("off 时不得执行")
		}
		if strings.Contains(why(id), "rule.observed") {
			t.Error("off 时不得有观察记录")
		}
	})

	step(t, "ACT-RULE-06 规则动作不再触发规则", func(t *testing.T) {
		writePack(t, packsDir, "acc-loop", map[string]string{"pack.yaml": accLoopPack})
		reload(e)
		for _, k := range []string{"acc-loop/event_rules/loop-a", "acc-loop/event_rules/loop-b"} {
			if r := e.Web("PATCH", modePath(k), `{"mode":"auto"}`); r.Code != 200 {
				t.Fatalf("Web 切 %s 为 auto: %d %s", k, r.Code, r.Body)
			}
		}
		before := eventCount(t, e)
		id := newItem("acc-loop")
		waitUntil(t, "第一层动作 set_phase=waiting 应发生", func() bool { return show(id).Phase == "waiting" })
		time.Sleep(60 * time.Second) // 验收要求触发后等 1 分钟，看是否出现循环
		it := show(id)
		if it.Phase != "waiting" {
			t.Errorf("第一层动作应已发生: %+v", it)
		}
		if it.has("acc-b") {
			t.Errorf("规则 A 的动作 Event 不应再命中规则 B: %+v", it)
		}
		if n := eventCount(t, e) - before; n > 6 {
			t.Errorf("事件数量 %d 过多，疑似循环", n)
		}
	})

	step(t, "ACT-RULE-07 规则内容变化后回到 observe", func(t *testing.T) {
		if r := e.Web("PATCH", modePath(tagKey), `{"mode":"auto"}`); r.Code != 200 {
			t.Fatalf("Web 切 auto: %d %s", r.Code, r.Body)
		}
		p := filepath.Join(packsDir, "acc-rule", "pack.yaml")
		if err := os.WriteFile(p, []byte(strings.Replace(accTagPack, "tag: acc-hit", "tag: acc-hit-v2", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		reload(e)
		if s := rules(t, e)["acc-tag"]; s.Mode != "observe" {
			t.Fatalf("内容变化后应回到 observe: %+v", s)
		}
		d := e.Web("GET", "/v1/deliveries/unread", "")
		if d.Code != 200 || !strings.Contains(d.Body, "health") {
			t.Errorf("应产出健康类投递: %d %s", d.Code, d.Body)
		}
		id := newItem("acc-rule")
		open(id)
		waitWhy(t, why, id, "rule.observed")
		if sh := show(id); sh.has("acc-hit") || sh.has("acc-hit-v2") {
			t.Errorf("回到 observe 后再次触发只应记录: %+v", sh)
		}
		waitWhy(t, why, id, "rule.observed")
	})
}

// settle 是"确认没有发生某事"前给异步规则执行的等待；wait 是等待"应当发生"的上限。
const (
	settle = 3 * time.Second
	wait   = 20 * time.Second
)

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(wait); ; time.Sleep(200 * time.Millisecond) {
		if ok() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s（等待 %s 仍未出现）", what, wait)
		}
	}
}

func waitWhy(t *testing.T, why func(string) string, id, sub string) (out string) {
	t.Helper()
	waitUntil(t, "apa why 应出现 "+sub, func() bool { out = why(id); return strings.Contains(out, sub) })
	return out
}

// eventCount 数事件流里的事件条数（apa events 每行一条）。
func eventCount(t *testing.T, e *baseline.Env) int {
	t.Helper()
	r := e.Apa(e.OBS, "", "events", "--limit", "100000")
	if r.Code != 0 {
		t.Fatalf("apa events: %d %s", r.Code, r.Stderr)
	}
	out := strings.TrimSpace(r.Stdout)
	if out == "" {
		return 0
	}
	return strings.Count(out, "\n") + 1
}
