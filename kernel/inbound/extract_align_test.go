package inbound

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// chatEvent 是一条按会话聚合、带三条消息的事件。
func chatEvent(id, typ string) map[string]any {
	return ev(id, typ, "群聊：发布 v2 文档", map[string]any{"sender": "bob", "items": []map[string]any{
		{"sender": "bob", "text": "MSG-ZERO 发布文档要改", "at": "2026-03-01T00:00:00Z"},
		{"sender": "bob", "text": "MSG-ONE 就是 v2 那份", "at": "2026-03-01T00:01:00Z"},
		{"sender": "bob", "text": "MSG-TWO 周末收拾阳台", "at": "2026-03-01T00:02:00Z"}}})
}

func (e *env) openIn(title, project string) string {
	e.t.Helper()
	var made struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": title, "project": project, "phase": "open"}), &made)
	return made.ID
}

// ACT-XAL-02：一条事件抽出两条候选，各归项目，按 refs 取正文，推荐补到清单里的事项。
func TestExtractCandidatesCarryProjectRefsAndAttach(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	x1 := e.openIn("发布 v2 文档", e.web)
	e.draft(fmt.Sprintf(`{"candidates":[
	 {"title":"改发布文档","kind":"task","summary":"s","project":%q,"recommend":"attach","attach_to":%q,"reason":"同一件事","refs":[0,1,9]},
	 {"title":"收拾阳台","kind":"task","project":%q,"refs":[2]}]}`, e.web, x1, e.mail))
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionProposed || len(d.Items) != 2 || d.Item != d.Items[0] {
		t.Fatalf("event %+v", d)
	}
	a, b := e.item(d.Items[0]), e.item(d.Items[1])
	if a.Project != e.web || a.Proposal == nil || a.Proposal.Recommend != "attach" || a.Proposal.AttachTo != x1 || a.Proposal.Reason != "同一件事" {
		t.Fatalf("A %+v %+v", a, a.Proposal)
	}
	if !strings.Contains(a.Body, "MSG-ZERO") || !strings.Contains(a.Body, "MSG-ONE") || strings.Contains(a.Body, "MSG-TWO") {
		t.Fatalf("A body %q", a.Body)
	}
	if b.Project != e.mail || !strings.Contains(b.Body, "MSG-TWO") || strings.Contains(b.Body, "MSG-ZERO") {
		t.Fatalf("B %+v", b)
	}
}

// ACT-XAL-04：清单外的项目不归项目、清单外的补到目标使推荐作废，候选保留，不报错。
func TestExtractDropsChoicesOutsideTheList(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"某事","kind":"task","project":"prj_nope","recommend":"attach","attach_to":"itm_nope","refs":[0]}]}`)
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionProposed || len(d.Items) != 1 {
		t.Fatalf("event %+v", d)
	}
	it := e.item(d.Item)
	if it.Project != "" || it.Proposal == nil || it.Proposal.AttachTo != "" || it.Proposal.Recommend != "" {
		t.Fatalf("item %+v %+v", it, it.Proposal)
	}
}

// ACT-XAL-07：只给 title/kind/summary 的旧输出仍被接受。
func TestExtractAcceptsLegacyThreeFieldOutput(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"旧输出","kind":"note","summary":"摘要"}]}`)
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	d := e.events("src_x")[0]
	it := e.item(d.Item)
	if d.Disposition != domain.DispositionProposed || it.Title != "旧输出" || it.Kind != "note" || it.Summary != "摘要" || it.Proposal.Recommend != "" {
		t.Fatalf("event %+v item %+v", d, it)
	}
}

// ACT-XAL-08：一条事件至多取 20 条候选。
func TestExtractTakesAtMostTwentyCandidates(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	var cs []string
	for i := range 25 {
		cs = append(cs, fmt.Sprintf(`{"title":"事 %d","kind":"task"}`, i))
	}
	e.draft(`{"candidates":[` + strings.Join(cs, ",") + `]}`)
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	if d := e.events("src_x")[0]; len(d.Items) != 20 || len(e.allItems()) != 20 {
		t.Fatalf("items %d / %d", len(d.Items), len(e.allItems()))
	}
}

// ACT-XAL-05：同一件事（规范化标题加发送者相同）再次抽出时并入已有建议，也不在一条事件内重复。
func TestExtractedCandidateMergesIntoExistingProposal(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"Ship Doc","kind":"task"},{"title":" ship  doc ","kind":"task"}]}`)
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	first := e.events("src_x")[0]
	if len(first.Items) != 1 || len(e.allItems()) != 1 {
		t.Fatalf("duplicate inside one event: %+v", first.Items)
	}
	e.draft(`{"candidates":[{"title":"SHIP DOC","kind":"task"}]}`)
	e.emit("src_x", "exits", chatEvent("c2", "x.chat"))
	if len(e.allItems()) != 1 {
		t.Fatalf("second extraction created a new proposal: %d items", len(e.allItems()))
	}
	second := e.byID("src_x")["c2"]
	it := e.item(first.Item)
	if second.Item != first.Item || second.Disposition != domain.DispositionProposed || len(it.Proposal.Sources) != 2 || it.Proposal.Sources[1] != second.ID {
		t.Fatalf("second %+v proposal %+v", second, it.Proposal)
	}
}

// ACT-XAL-03：预筛、抽取共用同一份清单；超过 100 条时截断并写明总数；外发范围为 none 的来源的事项不进外部端点的清单。
func TestSharedBoundedOpenItemsList(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"某事","kind":"task"}]}`)
	x1 := e.openIn("发布 v2 文档", e.web)
	for i := range 130 {
		e.openIn(fmt.Sprintf("杂项 %d", i), "")
	}
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	var body string
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, "抽取可以成为建议的候选事项") {
			body = r.BodyText
		}
	}
	// 请求体里的结构化输入是转义过的 JSON 文本，直接按键名取片段验证。
	if !strings.Contains(body, "open_items_total") || !strings.Contains(body, "131") || !strings.Contains(body, x1) || !strings.Contains(body, "projects") {
		t.Fatalf("extract request lacks the list: %s", body)
	}
	if n := strings.Count(body, `title`); n > 100+5 {
		t.Fatalf("list not bounded: %d titles", n)
	}
	batches := e.cls.Batches()
	if len(batches) == 0 || !strings.Contains(batches[0].BodyText, "未关闭事项") || !strings.Contains(batches[0].BodyText, "发布 v2 文档") {
		t.Fatalf("prescreen did not carry the list: %v", batches)
	}
}

func TestOpenItemsListSkipsItemsFromSourcesWithoutOutboundScope(t *testing.T) {
	e := newEnv(t, true)
	e.chatOnly(`{"candidates":[{"title":"某事","kind":"task"}]}`)
	e.emit("src_sec", "ticket", ev("n1", "ticket.noise", "噪声", nil))
	e.outbound("src_sec", "full", "none")
	e.must("item.create", map[string]any{"kind": "task", "title": "SECRET-TITLE", "phase": "open", "origin": map[string]any{"source": "src_sec"}})
	e.openIn("PLAIN-TITLE", "")
	e.locality("external")
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	e.outbound("src_x", "full", "full")
	e.emit("src_x", "exits", chatEvent("c2", "x.chat"))
	got := e.sent(0)
	if !strings.Contains(got, "PLAIN-TITLE") || strings.Contains(got, "SECRET-TITLE") {
		t.Fatalf("external endpoint list wrong: %s", got)
	}
}

// ACT-XAL-06：prescreen: none 不预筛、不产生预筛判断记录；对话候选捕获始终预筛。
func TestPrescreenNoneSkipsPrescreen(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"直接抽取","kind":"task"}]}`)
	e.emit("src_x", "exits", chatEvent("c1", "x.chat_np"))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionProposed || d.Judgment != "" || len(e.cls.Batches()) != 0 || e.extractReqs() != 1 {
		t.Fatalf("event %+v batches %d", d, len(e.cls.Batches()))
	}
	e.setCapture(e.web, true)
	e.submit("cap-np 下周三前把合同发给小王")
	if len(e.cls.Batches()) == 0 {
		t.Fatal("conversation capture skipped the prescreen")
	}
}
