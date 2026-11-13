//go:build e2e

package events

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// registered 是附录 B.4 的标准事件类型清单。
var registered = []string{"item.created", "item.updated", "item.phase_changed", "item.deadline_passed", "decision.requested",
	"decision.resolved", "delegation.created", "delegation.reported", "command.rejected", "material.revised",
	"material.revision_proposed", "guideline.activated", "project.updated", "project.allowed_executors_changed",
	"rule.mode_changed", "allowlist.changed", "policy.changed", "grant.issued", "grant.revoked", "run.started", "run.finished", "source.failed", "health.alert",
	// v0.2.0 新增（附录 B.4）：登录审计、进展、建议挂靠。
	"auth.login", "auth.login_locked", "auth.password_changed", "auth.session_revoked", "item.progress_added", "proposal.attached"}

func find(es []envelope, typ, object string) *envelope {
	for i := range es {
		if es[i].Type == typ && es[i].Object == object {
			return &es[i]
		}
	}
	return nil
}

// TestFlowEventSubscribe 按 flow-event-subscribe 的顺序执行 ACT-EVT-01..04，后一步依赖前一步的游标。
func TestFlowEventSubscribe(t *testing.T) {
	f := setup(t)
	_, base := f.feed(f.OBS, 0) // 环境搭建期间已有事件，订阅从此刻开始

	var last int64
	t.Run("ACT-EVT-01 跟随事件流", func(t *testing.T) {
		follow := f.start([]string{"APA_TOKEN=" + f.OBS}, "events", "--follow", "--cursor", itoa(base))

		const secretBody, secretReason, secretEvidence = "SECRET-BODY-不得进信封", "SECRET-REASON-不得进信封", "SECRET-EVIDENCE-不得进信封"
		id := f.task("evt-01 标题可见", "--body", secretBody, "--reason", secretReason)
		f.apa(f.DEL, 0, "item", "update", id, "--summary", "s", "--reason", secretReason, "--evidence", secretEvidence)
		f.transition(id, "done")

		eventually(t, 20*time.Second, "item.created 与 item.phase_changed 到达 apa events --follow", func() bool {
			es := lines(follow.out.String())
			return find(es, "item.created", id) != nil && find(es, "item.phase_changed", id) != nil
		})
		out := follow.out.String()
		for _, secret := range []string{secretBody, secretReason, secretEvidence} {
			if strings.Contains(out, secret) {
				t.Errorf("信封带了不该出现的内容 %q:\n%s", secret, out)
			}
		}
		es := lines(out)
		for _, typ := range []string{"item.created", "item.phase_changed"} {
			if e := find(es, typ, id); e.Title != "evt-01 标题可见" {
				t.Errorf("%s 信封标题 = %q，期望事项标题", typ, e.Title)
			}
		}
		for i, l := range strings.Split(strings.TrimSpace(out), "\n") {
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("第 %d 行不是 JSON 信封: %q", i, l)
			}
			for k := range m {
				if !slices.Contains([]string{"seq", "type", "object", "title", "actor", "at"}, k) {
					t.Errorf("信封多出字段 %q: %s", k, l)
				}
			}
			for _, k := range []string{"seq", "type", "object", "actor", "at"} {
				if _, ok := m[k]; !ok {
					t.Errorf("信封缺字段 %q: %s", k, l)
				}
			}
		}
		for i := 1; i < len(es); i++ {
			if es[i].Seq <= es[i-1].Seq {
				t.Errorf("序号没有递增: %d 之后是 %d", es[i-1].Seq, es[i].Seq)
			}
		}
		if es[0].Seq <= base {
			t.Errorf("--cursor %d 之后的第一条序号 = %d", base, es[0].Seq)
		}
		last = es[len(es)-1].Seq
		follow.stop()
	})
	if t.Failed() {
		t.Fatal("ACT-EVT-01 未通过，后续步骤不得开始")
	}

	t.Run("ACT-EVT-02 从游标续读", func(t *testing.T) {
		// 跟随已停止；产生三条（登记类型的）变更：建一个事项、改两次标题。
		id := f.task("evt-02")
		f.retitle(id, "evt-02 一")
		f.retitle(id, "evt-02 二")

		want, _ := f.feed(f.OBS, last)
		if len(want) != 3 {
			t.Fatalf("游标 %d 之后账本里应恰好有三条登记事件，实际 %d: %+v", last, len(want), want)
		}
		r := f.apa(f.OBS, 0, "events", "--follow", "--cursor", itoa(last), "--limit", "3")
		got := lines(r.Stdout)
		if len(got) != 3 {
			t.Fatalf("续读应恰好补上三条，实际 %d:\n%s", len(got), r.Stdout)
		}
		for i := range got {
			if got[i].Seq != want[i].Seq || got[i].Type != want[i].Type || got[i].Object != id {
				t.Errorf("第 %d 条 = %+v，期望 %+v（事项 %s）", i, got[i], want[i], id)
			}
		}
		if got[0].Type != "item.created" || got[1].Type != "item.updated" || got[2].Type != "item.updated" {
			t.Errorf("类型顺序不对: %s %s %s", got[0].Type, got[1].Type, got[2].Type)
		}
		// 以最后一条的序号继续读，不再重复。
		again := f.apa(f.OBS, 0, "events", "--cursor", itoa(got[2].Seq)).Stdout
		if len(lines(again)) != 0 {
			t.Errorf("越过最后一条后仍有事件: %s", again)
		}
	})

	t.Run("ACT-EVT-03 API 读取和权限", func(t *testing.T) {
		r := f.APIRaw("GET", "/v1/events?cursor=0", "", f.OBS)
		var page struct {
			Events []envelope `json:"events"`
		}
		if r.Code != 200 || json.Unmarshal([]byte(r.Body), &page) != nil || len(page.Events) == 0 {
			t.Errorf("OBS 读 /v1/events?cursor=0: HTTP %d %s", r.Code, r.Body)
		}
		// 长轮询：游标在末尾时挂起，新事件一到立即返回。
		_, end := f.feed(f.OBS, 0)
		type res struct {
			code int
			body string
		}
		ch := make(chan res, 1)
		go func() {
			r := f.APIRaw("GET", "/v1/events?cursor="+itoa(end)+"&wait=30", "", f.OBS)
			ch <- res{r.Code, r.Body}
		}()
		select {
		case r := <-ch:
			t.Fatalf("没有新事件时长轮询不该返回: %d %s", r.code, r.body)
		case <-time.After(1500 * time.Millisecond):
		}
		id := f.task("evt-03")
		select {
		case r := <-ch:
			if r.code != 200 || !strings.Contains(r.body, id) {
				t.Errorf("长轮询结果不对: %d %s", r.code, r.body)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("新事件产生后长轮询没有返回")
		}
		if r := f.APIRaw("GET", "/v1/events?cursor=0", "", ""); r.Code != 401 {
			t.Errorf("不带 token 读事件: HTTP %d（期望 401）%s", r.Code, r.Body)
		}
		if r := f.APIRaw("GET", "/v1/events?cursor=0", "", "not-a-real-token"); r.Code != 401 {
			t.Errorf("伪造 token 读事件: HTTP %d（期望 401）", r.Code)
		}
	})

	t.Run("ACT-EVT-04 只开放登记类型", func(t *testing.T) {
		// 让账本里出现更多种类的事件：决策请求、项目与策略变化。
		id := f.task("evt-04")
		f.apa(f.WRK, 0, "ask", id, "--question", "选哪个", "--option", "A=甲", "--option", "B=乙", "--recommend", "A")
		f.webOK("PUT", "/settings/projects/"+f.Projects["acc-open"]+"/allowed-executors", `{"executors":["work-host.cli"]}`, 200)

		es, _ := f.feed(f.OBS, 0)
		seen := map[string]bool{}
		for _, e := range es {
			seen[e.Type] = true
			if !slices.Contains(registered, e.Type) {
				t.Errorf("出现未登记类型 %q（seq %d）", e.Type, e.Seq)
			}
		}
		for _, must := range []string{"item.created", "item.updated", "item.phase_changed", "decision.requested"} {
			if !seen[must] {
				t.Errorf("账本里发生过 %s，但事件流没有它；已见类型 %v", must, seen)
			}
		}
	})
}
