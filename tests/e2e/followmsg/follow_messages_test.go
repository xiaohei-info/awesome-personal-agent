//go:build e2e

package followmsg

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// ctxMessages 是 `apa context --scope item` 里某事项的挂靠消息组。
type ctxMessages struct {
	Cursor   string `json:"cursor"`
	Item     string `json:"item"`
	Messages []struct {
		ID             string `json:"id"`
		Source         string `json:"source"`
		Sender         string `json:"sender"`
		SenderVerified bool   `json:"sender_verified"`
		Subject        string `json:"subject"`
	} `json:"messages"`
}

func (k *kit) itemContext(token, item string) (ctxMessages, bool) {
	k.t.Helper()
	r := k.Apa(token, "", "context", "--scope", "item", "--item", item)
	k.expect("apa context --scope item", r, 0)
	var out struct {
		Messages []ctxMessages `json:"messages"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatalf("context 输出不是 JSON: %q", r.Stdout)
	}
	for _, g := range out.Messages {
		if g.Item == item && len(g.Messages) > 0 {
			return g, true
		}
	}
	return ctxMessages{}, false
}

// itemView 是 `apa item show` 的事项字段。
type itemView struct {
	Phase string `json:"phase"`
	Title string `json:"title"`
}

func (k *kit) showItem(id string) itemView {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "item", "show", id)
	k.expect("item show "+id, r, 0)
	var out struct {
		Item itemView `json:"item"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Item
}

// judgeSaw 报告 MODEL_STUB 是否收到过同时含 subject 与 hint 的判断请求（关联线索随判断输入带走）。
func (k *kit) judgeSaw(subject, hint string) bool {
	for _, r := range k.ModelStub.Requests() {
		if strings.Contains(r.BodyText, subject) && strings.Contains(r.BodyText, hint) {
			return true
		}
	}
	return false
}

func (k *kit) itemCount() int {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "item", "list")
	k.expect("item list", r, 0)
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	return len(out.Items)
}

// delegated 在 acc-work 建一个委派子事项（assignee=work-host.cli），并由 WRK 回执 PR，使 PR 登记为关联键。
func (k *kit) delegated(title, pr string) string {
	k.t.Helper()
	parent := k.idOf("item add", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", title, "--phase", "open",
		"--project", k.Projects["acc-work"], "--evidence", "e2e 准备父事项"))
	child := k.idOf("delegate", k.Apa(k.DEL, "", "delegate", "--parent", parent, "--assignee", workHost, "--evidence", "e2e 委派"))
	k.expect("report --pr", k.Apa(k.WRK, "", "report", child, "--outcome", "progress", "--summary", "已开 PR", "--pr", pr), 0)
	return child
}

// TestFlowFollowMessages 按验收文档 flow-follow-messages 的顺序执行 ACT-MSG-01..09。
func TestFlowFollowMessages(t *testing.T) {
	k := newKit(t)
	k.ModelStub.SetDefault(modelstub.Reply{Content: `{"choice":"task","confidence":0.9}`})
	const pr1 = "https://example.invalid/acc/test-repo/pull/1"
	var child, msg1, msg2 string // msg* 是挂靠消息（来源事件）的内部 id

	k.step("ACT-MSG-01 准备已委派事项和关联键", func(t *testing.T) {
		child = k.delegated("跟进消息验收", pr1)
		if got := k.showItem(child).Title; got == "" {
			t.Fatal("子事项读不出来")
		}
		// 同一个 PR 不能登记在第二个事项上，说明它已作为关联键登记在 child 上。
		other := k.idOf("item add", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", "另一个", "--phase", "open",
			"--project", k.Projects["acc-work"], "--evidence", "e2e"))
		dup := k.idOf("delegate", k.Apa(k.DEL, "", "delegate", "--parent", other, "--assignee", workHost, "--evidence", "e2e 委派"))
		k.expect("同一 PR 登记到另一事项", k.Apa(k.WRK, "", "report", dup, "--outcome", "progress", "--summary", "x", "--pr", pr1), exitConflict)
	})

	k.step("ACT-MSG-02 新评论挂靠原事项", func(t *testing.T) {
		before := k.itemCount()
		id := k.emit("comment.created", "PR 新评论：请补测试", pr1)
		ev := k.settled(id)
		if ev.Disposition != "attached" || ev.AttachState != "pending" || ev.Item != child {
			t.Fatalf("应挂靠 %s 且 pending: %+v", child, ev)
		}
		if n := k.stubRequestsAbout("PR 新评论：请补测试"); n != 0 {
			t.Errorf("挂靠不进规则和判断，MODEL_STUB 不应有关于该评论的请求，实际 %d", n)
		}
		if got := k.itemCount(); got != before {
			t.Errorf("不应生成新事项: %d → %d", before, got)
		}
		msg1 = ev.ID
	})

	var cursor string
	k.step("ACT-MSG-03 增量转给负责执行者", func(t *testing.T) {
		g, ok := k.itemContext(k.WRK, child)
		if !ok || len(g.Messages) != 1 {
			t.Fatalf("apa context 应带这条消息: %+v", g)
		}
		m := g.Messages[0]
		if m.ID != msg1 || m.Source != srcID || m.Sender != "alice" || !strings.Contains(m.Subject, "请补测试") {
			t.Errorf("消息缺来源、发送者或内容: %+v", m)
		}
		r := k.Apa(k.WRK, "", "context", "--scope", "item", "--item", child, "--format", "inject")
		k.expect("context --format inject", r, 0)
		if !strings.Contains(r.Stdout, "请补测试") {
			t.Errorf("注入文本缺消息内容: %s", r.Stdout)
		}
		if ev, _ := k.event("ev-1"); ev.AttachState != "forwarded" {
			t.Errorf("attach_state 应变 forwarded: %+v", ev)
		}
		cursor = g.Cursor
		if cursor == "" {
			t.Fatal("负责者应拿到确认游标")
		}
	})

	k.step("ACT-MSG-04 回执确认后算已处理", func(t *testing.T) {
		k.expect("report --ack-cursor", k.Apa(k.WRK, "", "report", child, "--outcome", "progress", "--summary", "已回复", "--ack-cursor", cursor), 0)
		if ev, _ := k.event("ev-1"); ev.AttachState != "processed" {
			t.Errorf("游标之前的消息应变 processed: %+v", ev)
		}
		if g, ok := k.itemContext(k.WRK, child); ok {
			t.Errorf("已处理的消息不应再转发: %+v", g)
		}
	})

	k.step("ACT-MSG-05 交回一次", func(t *testing.T) {
		id := k.emit("comment.created", "PR 评论：这条不该给我", pr1)
		ev := k.settled(id)
		if ev.AttachState != "pending" || ev.Item != child {
			t.Fatalf("第二条消息应挂靠: %+v", ev)
		}
		msg2 = ev.ID
		if _, ok := k.itemContext(k.WRK, child); !ok {
			t.Fatal("交回前应能转发")
		}
		before := len(k.ModelStub.Requests())
		k.expect("handback", k.Apa(k.WRK, "", "handback", msg2, "--reason", "不归我"), 0)
		k.waitFor("交回的消息进判断流", 20*1e9, func() bool { return len(k.ModelStub.Requests()) > before })
		ev, _ = k.event(id)
		if ev.AttachState != "handed_back" {
			t.Errorf("attach_state 应为 handed_back: %+v", ev)
		}
		k.expect("第二次 handback", k.Apa(k.WRK, "", "handback", msg2, "--reason", "再来一次"), exitConflict)
		if g, ok := k.itemContext(k.WRK, child); ok {
			t.Errorf("交回的消息不应转回该执行者: %+v", g)
		}
	})

	k.step("ACT-MSG-06 关闭时未处理消息退回", func(t *testing.T) {
		id := k.emit("comment.created", "PR 评论：合并前最后一条", pr1)
		if ev := k.settled(id); ev.AttachState != "pending" || ev.Item != child {
			t.Fatalf("应挂靠且未处理: %+v", ev)
		}
		before := len(k.ModelStub.Requests())
		k.expect("transition done", k.Apa(k.DEL, "", "item", "transition", child, "--to", "done", "--reason", "完成", "--evidence", "e2e 关闭子事项"), 0)
		k.waitFor("退回的消息进判断流", 20*1e9, func() bool { return len(k.ModelStub.Requests()) > before })
		ev, _ := k.event(id)
		if ev.AttachState != "released" {
			t.Errorf("attach_state 应为 released: %+v", ev)
		}
		k.waitFor("退回的消息有处置", 20*1e9, func() bool { e, _ := k.event(id); return e.Disposition != "attached" && e.Disposition != "pending" })
		if ev, _ = k.event(id); ev.Disposition != "proposed" || ev.Item == "" || ev.Item == child {
			t.Errorf("退回的消息不丢，应经判断流成为建议: %+v", ev)
		}
	})

	k.step("ACT-MSG-07 只命中已关闭事项时不猜", func(t *testing.T) {
		before := len(k.ModelStub.Requests())
		id := k.emit("comment.created", "PR 合并后的追问", pr1)
		k.waitFor("事件有处置", 20*1e9, func() bool { e, ok := k.event(id); return ok && e.Disposition == "proposed" })
		ev, _ := k.event(id)
		if ev.Item == "" || ev.Item == child || ev.AttachState != "" {
			t.Errorf("不应挂靠已关闭事项: %+v", ev)
		}
		if len(k.ModelStub.Requests()) <= before {
			t.Error("应走普通判断流")
		}
		if got := k.showItem(child).Phase; got != "done" {
			t.Errorf("不应重开原事项，phase=%s", got)
		}
		if !k.judgeSaw("PR 合并后的追问", "matches only closed items") {
			t.Error("判断型的输入应附关联线索")
		}
	})

	const pr2, pr3 = "https://example.invalid/acc/test-repo/pull/2", "https://example.invalid/acc/test-repo/pull/3"
	var childB, childC string
	k.step("ACT-MSG-08 命中多个未关闭事项时不猜", func(t *testing.T) {
		childB, childC = k.delegated("多命中 B", pr2), k.delegated("多命中 C", pr3)
		// 同一个 PR 只能登记在一个事项上，所以用一条带两个关联键的事件同时命中两个未关闭事项。
		id := k.emit("comment.created", "同时提到两个 PR", pr2, pr3)
		k.waitFor("事件有处置", 20*1e9, func() bool { e, ok := k.event(id); return ok && e.Disposition == "proposed" })
		ev, _ := k.event(id)
		if ev.Item == childB || ev.Item == childC || ev.Item == "" || ev.AttachState != "" {
			t.Errorf("不应挂靠任一事项: %+v", ev)
		}
		if !k.judgeSaw("同时提到两个 PR", "matches several open items") {
			t.Error("判断型的输入应附关联线索")
		}
		for _, c := range []string{childB, childC} {
			if _, ok := k.itemContext(k.WRK, c); ok {
				t.Errorf("%s 不应收到消息", c)
			}
		}
	})

	k.step("ACT-MSG-09 其他 worker 不能交回", func(t *testing.T) {
		id := k.emit("comment.created", "只命中 B", pr2)
		ev := k.settled(id)
		if ev.AttachState != "pending" || ev.Item != childB {
			t.Fatalf("应挂靠 B: %+v", ev)
		}
		k.expect("WRK2 handback", k.Apa(k.WRK2, "", "handback", ev.ID, "--reason", "越权"), exitDenied)
		if got, _ := k.event(id); got.AttachState != "pending" {
			t.Errorf("越权交回不应生效: %+v", got)
		}
	})

	echoSteps(k)
}
