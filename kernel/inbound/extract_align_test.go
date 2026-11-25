package inbound

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
		if strings.Contains(r.BodyText, "确需本人后续处理或拍板的候选事项") {
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

// inputOf 从生成端点的请求体里取出结构化输入（"## 结构化输入" 之后的那一段 JSON）。
func inputOf(t *testing.T, body string) map[string]any {
	t.Helper()
	var req struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	const mark = "## 结构化输入\n"
	for _, m := range req.Messages {
		i := strings.Index(m.Content, mark)
		if i < 0 {
			continue
		}
		rest := m.Content[i+len(mark):]
		if j := strings.Index(rest, "\n## "); j >= 0 {
			rest = rest[:j]
		}
		var in map[string]any
		if err := json.Unmarshal([]byte(rest), &in); err != nil {
			t.Fatalf("structured input: %v", err)
		}
		return in
	}
	t.Fatalf("no structured input in request: %s", body)
	return nil
}

// listTitles 取结构化输入里某份清单的标题（recent_done、active_proposals）。
func listTitles(m map[string]any, key string) (out []string) {
	xs, _ := m[key].([]any)
	for _, x := range xs {
		if it, ok := x.(map[string]any); ok {
			out = append(out, fmt.Sprint(it["title"]))
		}
	}
	return
}

// extractBody 是生成端点收到的抽取请求体（最后一条，含候选抽取的模板问法）。
func extractBody(e *env) string {
	rs := e.stub.Requests()
	for i := len(rs) - 1; i >= 0; i-- {
		if strings.Contains(rs[i].BodyText, "确需本人后续处理或拍板的候选事项") {
			return rs[i].BodyText
		}
	}
	return ""
}

// 抽取前的去重清单（§9.3）：最近 7 天已完成的事项与当前仍待处理的建议随预筛资料与抽取输入一起给模型；
// 出自外发范围未选的来源的不同（§14.14）。
func TestListsCarryRecentDoneAndActiveProposals(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"新事","kind":"task"}]}`)
	e.emit("src_sec", "ticket", ev("n1", "ticket.noise", "噪声", nil))
	e.outbound("src_sec", "full", "none")
	e.emit("src_x", "exits", chatEvent("c0", "x.chat")) // 先登记来源，再给它选外发范围
	e.outbound("src_x", "full", "full")
	done := e.openIn("DONE-MARK 归档旧项目", "")
	e.must("item.transition", map[string]any{"id": done, "to": "done", "reason": "finished"})
	var hidden struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "OLD-DONE 范围外的", "phase": "open",
		"origin": map[string]any{"source": "src_sec"}}), &hidden)
	e.must("item.transition", map[string]any{"id": hidden.ID, "to": "done", "reason": "finished"})
	e.must("item.create", map[string]any{"kind": "task", "title": "PROP-MARK 装修报价", "phase": "proposed"})
	e.locality("external")
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	body := extractBody(e)
	if body == "" {
		t.Fatal("extraction was not asked")
	}
	in := inputOf(t, body)
	if got := listTitles(in, "recent_done"); len(got) != 1 || got[0] != "DONE-MARK 归档旧项目" {
		t.Fatalf("recent_done %v", got)
	}
	if got := listTitles(in, "active_proposals"); !slices.Contains(got, "PROP-MARK 装修报价") {
		t.Fatalf("active_proposals %v", got)
	}
	batches := e.cls.Batches()
	if len(batches) == 0 {
		t.Fatal("prescreen was not asked")
	}
	last := batches[len(batches)-1].BodyText
	for _, want := range []string{"最近 7 天已完成事项", "当前待处理建议", "DONE-MARK", "PROP-MARK"} {
		if !strings.Contains(last, want) {
			t.Fatalf("prescreen material lacks %s: %s", want, last)
		}
	}
}

// 去重清单有上限：最近已完成至多 30 条、待处理建议至多 50 条，同项目的排在前面。
func TestListsBoundRecentDoneAndActiveProposals(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"新事","kind":"task"}]}`)
	e.emit("src_x", "exits", chatEvent("c0", "x.chat"))
	for i := range 35 {
		id := e.openIn(fmt.Sprintf("DONE-%d", i), "")
		e.must("item.transition", map[string]any{"id": id, "to": "done", "reason": "finished"})
	}
	for i := range 55 {
		e.must("item.create", map[string]any{"kind": "task", "title": fmt.Sprintf("PROP-%d", i), "phase": "proposed"})
	}
	e.emit("src_x", "exits", chatEvent("c1", "x.chat"))
	body := extractBody(e)
	if body == "" {
		t.Fatal("extraction was not asked")
	}
	in := inputOf(t, body)
	if done := listTitles(in, "recent_done"); len(done) != 30 {
		t.Fatalf("recent_done was not bounded to 30: %d", len(done))
	}
	if props := listTitles(in, "active_proposals"); len(props) != 50 {
		t.Fatalf("active_proposals was not bounded to 50: %d", len(props))
	}
}

// 截断时同项目的排前，其余按时间新到旧。
func TestPickRecentPrefersTheSameProject(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	list := []recentItem{
		{ID: "new-other", Project: "p2", at: base.Add(3 * time.Hour)},
		{ID: "mine-old", Project: "p1", at: base},
		{ID: "mid-other", Project: "p2", at: base.Add(2 * time.Hour)},
	}
	var sn snapshot
	got := sn.pickRecent(list, "p1", "", 2)
	if len(got) != 2 || got[0].ID != "mine-old" || got[1].ID != "new-other" {
		t.Fatalf("same-project first, then newest: %v", got)
	}
	if got := sn.pickRecent(list, "", "", 3); len(got) != 3 || got[0].ID != "new-other" || got[2].ID != "mine-old" {
		t.Fatalf("newest first without a project: %v", got)
	}
}
