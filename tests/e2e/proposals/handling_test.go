//go:build e2e

package proposals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	srcA = "src_prh_a" // PRH_A（来源 id 须形如 src_<小写名>）
	srcB = "src_prh_b" // PRH_B
)

// ---- 读取内核状态 ----

func (k *kit) strs(v any) []string { return k.stringsOf(v) }

// getItem 是 `GET /v1/items/{id}`（本人会话），返回 item 对象。
func (k *kit) getItem(id string) map[string]any {
	k.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/items/"+id, "", 200)), &out))
	if out.Item == nil {
		k.t.Fatalf("GET /items/%s 没有 item", id)
	}
	return out.Item
}

// messages 是 `GET /v1/items/{id}/messages` 里挂靠到该事项的来源事件标题。
func (k *kit) messages(id string) []string {
	k.t.Helper()
	var out struct {
		Messages []struct {
			Subject string `json:"subject"`
			ID      string `json:"id"`
		} `json:"messages"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/items/"+id+"/messages", "", 200)), &out))
	var subs []string
	for _, m := range out.Messages {
		subs = append(subs, m.Subject)
	}
	return subs
}

// progress 是事项上的进展事件（item.progress_added）。
func (k *kit) progress(id string) []step { return k.events(id, "item.progress_added") }

// attachedEvents 是以 target 为对象、补入了 prop 的 proposal.attached 事件。
func (k *kit) attachedEvents(target, prop string) []step {
	var out []step
	for _, e := range k.events(target, "proposal.attached") {
		if e.Payload["proposal"] == prop {
			out = append(out, e)
		}
	}
	return out
}

// full 是 `apa item show` 的完整输出：item、relations、progress、correlation_keys。
func (k *kit) full(id string) map[string]any {
	k.t.Helper()
	var out map[string]any
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "item", "show", id).Stdout), &out))
	return out
}

// relatesTo 报告 from 与 to 之间是否有 relates 关系。
func (k *kit) relatesTo(from, to string) bool {
	rels, _ := k.full(from)["relations"].([]any)
	for _, r := range rels {
		m, _ := r.(map[string]any)
		if m["type"] == "relates" && (m["from"] == from && m["to"] == to || m["from"] == to && m["to"] == from) {
			return true
		}
	}
	return false
}

func (k *kit) correlationKeys(id string) []string {
	var keys []string
	l, _ := k.full(id)["correlation_keys"].([]any)
	for _, x := range l {
		m, _ := x.(map[string]any)
		keys = append(keys, m["key"].(string))
	}
	return keys
}

func (k *kit) stream() map[string]map[string]any {
	items, _ := k.streamItems()
	m := map[string]map[string]any{}
	for _, it := range items {
		m[it["id"].(string)] = it
	}
	return m
}

// sourcesOf 是建议 proposal.sources（来源事件 id）。
func (k *kit) sourcesOf(prop string) []string { return k.strs(k.proposalOf(prop)["sources"]) }

// sevID 是来源事件在账本里的 id（proposal.sources 里记的是它）。
func (k *kit) sevID(src, eventID string) string {
	ev, ok := k.srcEvent(src, eventID)
	if !ok {
		k.t.Fatalf("来源 %s 没有事件 %s", src, eventID)
	}
	return ev.ID
}

func (k *kit) disposals(since int64) []step {
	var out []step
	for _, e := range k.events("", "") {
		if e.Seq > since && (e.Type == "item.phase_changed" || e.Type == "proposal.attached") && e.Actor.Type != "" {
			out = append(out, e)
		}
	}
	return out
}

// batchResult 是 POST /proposals/batch 的一条结果。
type batchResult struct {
	ID   string `json:"id"`
	Do   string `json:"do"`
	OK   bool   `json:"ok"`
	Code string `json:"code"`
	Why  string `json:"why"`
}

func parseResults(t *testing.T, body string) map[string]batchResult {
	t.Helper()
	var out struct {
		Results []batchResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("批量处理的输出不是 {results}: %q", body)
	}
	m := map[string]batchResult{}
	for _, r := range out.Results {
		m[r.ID] = r
	}
	return m
}

func (k *kit) allProposalIDs() []string {
	var ids []string
	for id := range k.stream() {
		ids = append(ids, id)
	}
	return ids
}

// ---- 事件订阅（ACT-PRH-18） ----

type envelope struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
}

func (k *kit) feedLines(token string, args ...string) []envelope {
	k.t.Helper()
	var es []envelope
	for _, l := range strings.Split(k.apa(token, 0, args...).Stdout, "\n") {
		var e envelope
		if strings.TrimSpace(l) != "" && json.Unmarshal([]byte(l), &e) == nil {
			es = append(es, e)
		}
	}
	return es
}

// head 是事件流当前游标（登记类型的最后一条事件序号）。
func (k *kit) head() int64 {
	k.t.Helper()
	var page struct {
		Next int64 `json:"next"`
	}
	r := k.APIRaw("GET", "/v1/events?cursor=0&limit=500", "", k.OBS)
	k.must(json.Unmarshal([]byte(r.Body), &page))
	for page.Next > 0 {
		var more struct {
			Events []envelope `json:"events"`
			Next   int64      `json:"next"`
		}
		r = k.APIRaw("GET", "/v1/events?cursor="+itoa(page.Next)+"&limit=500", "", k.OBS)
		k.must(json.Unmarshal([]byte(r.Body), &more))
		if len(more.Events) == 0 {
			break
		}
		page.Next = more.Next
	}
	return page.Next
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ---- 流程 ----

// TestFlowProposalHandling 按验收文档 flow-proposal-handling 的顺序执行 ACT-PRH-01..18：
// 同一件事只成为一条建议，可补到已有事项，可按推荐批量处理且每条可追溯、可撤销。
//
// 周期缩短：来源周期每秒（文档为每分钟），"等两个周期"按事件被处置为准；合并窗口的 8 天用测试时钟 tc_advance。
func TestFlowProposalHandling(t *testing.T) {
	k := newKit(t)
	var (
		m1, t3, pa, pb, pc, c0 string
		e1, e2, e3             string
		b                      [5]string
		pTitleC, pOld          string
		s0                     int64
	)

	k.step("ACT-PRH-01 登记两个脚本来源并配置替身", func(t *testing.T) {
		k.registerSource(srcA)
		k.registerSource(srcB)
		// 具体的关键字先于通用关键字（首个命中生效）。
		k.screen("prh-unsure", 0.35)
		k.unsure("prh-unsure")
		k.screen("prh", 0.90)
		k.ModelStub.Judge(judgeYes("prh"))
		out := k.apa(k.OBS, 0, "source", "list").Stdout
		for _, id := range []string{srcA, srcB} {
			if !strings.Contains(out, `"id":"`+id+`"`) {
				t.Errorf("apa source list 缺 %s: %s", id, out)
			}
		}
		eq(t, "default_project 出现次数", strings.Count(out, `"default_project":"`+k.Projects["acc-open"]+`"`), 2)
	})

	k.step("ACT-PRH-02 两个来源带同一链接的事件合并为一条建议", func(t *testing.T) {
		k.draftDefault("prh 季度会议安排")
		k.draftDefault("prh 季度会议（转发）")
		k.emit(srcA, Ev{ID: "prh-link-a", Subject: "prh 季度会议安排", Link: "https://example.invalid/meet/1"})
		first := k.settled(srcA, "prh-link-a")
		k.emit(srcB, Ev{ID: "prh-link-b", Subject: "prh 季度会议（转发）", Link: "https://EXAMPLE.invalid/meet/1/"})
		second := k.settled(srcB, "prh-link-b")
		if first.Disposition != "proposed" || second.Disposition != "proposed" {
			t.Fatalf("两条事件都应 proposed: %+v %+v", first, second)
		}
		if first.MergeKey == "" || first.MergeKey != second.MergeKey {
			t.Errorf("两条来源事件的 merge_key 应相同且非空: %q %q", first.MergeKey, second.MergeKey)
		}
		eq(t, "item 指向同一事项", second.Item, first.Item)
		m1 = first.Item
		srcs := k.sourcesOf(m1)
		if !slices.Contains(srcs, first.ID) || !slices.Contains(srcs, second.ID) || len(srcs) != 2 {
			t.Errorf("proposal.sources 应同时含 prh-link-a（%s）与 prh-link-b（%s）: %v", first.ID, second.ID, srcs)
		}
		var n int
		for _, it := range k.stream() {
			if strings.Contains(it["title"].(string), "季度会议") {
				n++
			}
		}
		eq(t, "建议流里的该条建议数", n, 1)
		listed := k.webOK("GET", "/items?phase=proposed", "", 200)
		eq(t, "GET /items?phase=proposed 里该建议出现次数", strings.Count(listed, `"id":"`+m1+`"`), 1)
	})

	k.step("ACT-PRH-03 没有链接时按规范化标题加发送者合并", func(t *testing.T) {
		k.draftDefault("prh 报销单待签")
		k.draftDefault("  PRH 报销单待签 ")
		k.emit(srcA, Ev{ID: "prh-title-a", Subject: "prh 报销单待签", Sender: "alice@example.invalid"})
		ta := k.settled(srcA, "prh-title-a")
		k.emit(srcB, Ev{ID: "prh-title-b", Subject: "  PRH 报销单待签 ", Sender: "alice@example.invalid"})
		tb := k.settled(srcB, "prh-title-b")
		k.emit(srcB, Ev{ID: "prh-title-c", Subject: "prh 报销单待签", Sender: "bob@example.invalid"})
		tc := k.settled(srcB, "prh-title-c")
		eq(t, "title-a 与 title-b 并入同一条建议", tb.Item, ta.Item)
		eq(t, "来源数", len(k.sourcesOf(ta.Item)), 2)
		if tc.Item == ta.Item || tc.Item == "" {
			t.Errorf("不同发送者的 title-c 应单独成为另一条建议: %+v", tc)
		}
		eq(t, "title-c 的建议来源数", len(k.sourcesOf(tc.Item)), 1)
		pTitleC = tc.Item
	})

	k.step("ACT-PRH-04 命中已确认的事项时不自动合并，只推荐补到该事项", func(t *testing.T) {
		k.webOK("POST", "/items/"+m1+"/accept", `{}`, 200)
		eq(t, "M1 阶段", k.phase(m1), "open")
		msgs0, prog0 := len(k.messages(m1)), len(k.progress(m1))
		k.draftDefault("prh 季度会议改期")
		k.emit(srcA, Ev{ID: "prh-link-c", Subject: "prh 季度会议改期", Link: "https://example.invalid/meet/1"})
		ev := k.settled(srcA, "prh-link-c")
		if ev.Disposition != "proposed" || ev.Item == "" || ev.Item == m1 {
			t.Fatalf("应新建一条建议而不是并入 M1: %+v", ev)
		}
		p := k.proposalOf(ev.Item)
		eq(t, "proposal.recommend", p["recommend"], "attach")
		eq(t, "proposal.attach_to", p["attach_to"], m1)
		eq(t, "M1 消息数", len(k.messages(m1)), msgs0)
		eq(t, "M1 进展数", len(k.progress(m1)), prog0)
		eq(t, "M1 阶段", k.phase(m1), "open")
	})

	k.step("ACT-PRH-05 已丢弃的建议不再接收合并", func(t *testing.T) {
		k.webOK("POST", "/items/"+pTitleC+"/dismiss", `{"reason":"不需要"}`, 200)
		eq(t, "title-c 的建议", k.phase(pTitleC), "dropped")
		k.draftDefault("prh 报销单待签")
		k.emit(srcA, Ev{ID: "prh-title-d", Subject: "prh 报销单待签", Sender: "bob@example.invalid"})
		td := k.settled(srcA, "prh-title-d")
		if td.Disposition != "proposed" || td.Item == "" || td.Item == pTitleC {
			t.Fatalf("title-d 应成为新的 proposed 建议: %+v", td)
		}
		eq(t, "title-d 的建议阶段", k.phase(td.Item), "proposed")
		eq(t, "已丢弃的建议", k.phase(pTitleC), "dropped")
		eq(t, "已丢弃建议的来源", len(k.sourcesOf(pTitleC)), 1)
	})

	k.step("ACT-PRH-06 超过 7 天的建议不再合并", func(t *testing.T) {
		k.draftDefault("prh 旧事")
		k.emit(srcA, Ev{ID: "prh-old-a", Subject: "prh 旧事", Link: "https://example.invalid/old/1"})
		pOld = k.proposalFrom(srcA, "prh-old-a")
		k.TCAdvance(691200) // 推进 8 天，同一 APA_HOME，不重启服务；时间不回拨，后续 action 以新时刻为准
		k.WebLogin()        // 8 天后原 Web 会话已过期，本人重新登录
		k.emit(srcB, Ev{ID: "prh-old-b", Subject: "prh 旧事", Link: "https://example.invalid/old/1"})
		other := k.settled(srcB, "prh-old-b")
		if other.Disposition != "proposed" || other.Item == "" || other.Item == pOld {
			t.Fatalf("prh-old-b 应生成另一条建议: %+v", other)
		}
		eq(t, "P_OLD 阶段", k.phase(pOld), "proposed")
		if srcs, a := k.sourcesOf(pOld), k.sevID(srcA, "prh-old-a"); len(srcs) != 1 || srcs[0] != a {
			t.Errorf("P_OLD 的 proposal.sources 应只有 prh-old-a（%s）: %v", a, srcs)
		}
	})

	k.step("ACT-PRH-07 建议展示推荐、理由与“把握低”", func(t *testing.T) {
		k.draft("prh-rec-dismiss", map[string]any{"title": "起草：prh-rec-dismiss", "summary": "群发", "kind": "task", "recommend": "dismiss", "reason": "群发通知，与你无关"})
		k.draftDefault("prh-unsure")
		k.emit(srcA, Ev{ID: "prh-rec-dismiss", Subject: "prh-rec-dismiss"})
		k.emit(srcA, Ev{ID: "prh-unsure", Subject: "prh-unsure"})
		r1, r2 := k.proposalFrom(srcA, "prh-rec-dismiss"), k.proposalFrom(srcA, "prh-unsure")
		p := k.getItem(r1)["proposal"].(map[string]any)
		eq(t, "recommend", p["recommend"], "dismiss")
		eq(t, "reason", p["reason"], "群发通知，与你无关")
		eq(t, "low_confidence", p["low_confidence"], false)
		if srcs, a := k.strs(p["sources"]), k.sevID(srcA, "prh-rec-dismiss"); len(srcs) != 1 || srcs[0] != a {
			t.Errorf("sources 应只有 prh-rec-dismiss（%s）: %v", a, srcs)
		}
		eq(t, "第二条 low_confidence", k.getItem(r2)["proposal"].(map[string]any)["low_confidence"], true)
	})

	k.step("ACT-PRH-08 delegate 附本人原话把建议补到已有事项", func(t *testing.T) {
		c0 = itoa(k.head()) // 事件游标 C0
		k.draftDefault("prh 合同第二版意见")
		k.emit(srcA, Ev{ID: "prh-att-1", Subject: "prh 合同第二版意见", Summary: "第二版的修改意见", Keys: []string{"prh-thread-9"}})
		pa = k.proposalFrom(srcA, "prh-att-1")
		t3 = k.openItem("prh 合同谈判")
		prog0 := len(k.progress(t3))
		paItem := k.item(pa)
		k.apa(k.DEL, 0, "attach", pa, "--to", t3, "--evidence", "这条补到合同谈判")
		eq(t, "PA 阶段", k.phase(pa), "dropped")
		if reason := k.last(pa, "item.phase_changed").Reason; !strings.Contains(reason, t3) {
			t.Errorf("PA 的 reason 应记并入 T3: %q", reason)
		}
		if !k.relatesTo(pa, t3) {
			t.Errorf("PA 与 T3 应有 relates 关系: %s", k.apa(k.OBS, 0, "item", "show", pa).Stdout)
		}
		if !slices.Contains(k.messages(t3), "prh 合同第二版意见") {
			t.Errorf("prh-att-1 应成为 T3 的挂靠消息: %v", k.messages(t3))
		}
		ps := k.progress(t3)
		eq(t, "T3 新增进展数", len(ps), prog0+1)
		want := paItem["title"].(string)
		if s, _ := paItem["summary"].(string); s != "" {
			want += "\n" + s
		}
		prog, _ := k.full(t3)["progress"].([]any)
		if last, _ := prog[len(prog)-1].(map[string]any); len(prog) != prog0+1 || last["text"] != want {
			t.Errorf("T3 新增的进展应是 PA 的标题与摘要 %q: %v", want, prog)
		}
		if !slices.Contains(k.correlationKeys(t3), "prh-thread-9") {
			t.Errorf("T3 应登记关联键 prh-thread-9: %v", k.correlationKeys(t3))
		}
		evs := k.attachedEvents(t3, pa)
		if len(evs) != 1 {
			t.Fatalf("应有一条 proposal.attached: %d", len(evs))
		}
		eq(t, "on_behalf_of", evs[0].Actor.OnBehalfOf, "owner")
		eq(t, "evidence", evs[0].Ev, "这条补到合同谈判")
		eq(t, "会话类型", evs[0].Actor.SessionKind, "direct")
		var why struct {
			Steps []step `json:"steps"`
		}
		k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "why", pa).Stdout), &why))
		last := why.Steps[len(why.Steps)-1]
		if last.Type != "item.phase_changed" || last.Actor.OnBehalfOf != "owner" || last.Actor.SessionKind != "direct" {
			t.Errorf("apa why PA 的最后一步应是带 on_behalf_of 与会话类型 direct 的并入: %+v", last)
		}
	})

	k.step("ACT-PRH-09 补到之后同一线程的新消息直接回到目标事项", func(t *testing.T) {
		k.clear()
		before := len(k.stream())
		k.emit(srcA, Ev{ID: "prh-att-2", Subject: "prh 合同第三版意见", Keys: []string{"prh-thread-9"}})
		ev := k.settled(srcA, "prh-att-2")
		eq(t, "disposition", ev.Disposition, "attached")
		eq(t, "item", ev.Item, t3)
		eq(t, "建议流条数", len(k.stream()), before)
		if !slices.Contains(k.messages(t3), "prh 合同第三版意见") {
			t.Errorf("prh-att-2 应出现在 T3 的消息列表: %v", k.messages(t3))
		}
		eq(t, "含 prh-att-2 的初筛请求", k.clsLogWith("prh 合同第三版意见"), 0)
		eq(t, "含 prh-att-2 的起草请求", k.modelLogWith("prh 合同第三版意见"), 0)
	})

	k.step("ACT-PRH-10 本人在 Web 把建议补到已有事项", func(t *testing.T) {
		k.draftDefault("prh 会议纪要")
		k.emit(srcB, Ev{ID: "prh-att-3", Subject: "prh 会议纪要"})
		pb = k.proposalFrom(srcB, "prh-att-3")
		prog0 := len(k.progress(t3))
		k.webOK("POST", "/items/"+pb+"/attach", `{"to":"`+t3+`"}`, 200)
		eq(t, "PB 阶段", k.phase(pb), "dropped")
		if !k.relatesTo(pb, t3) {
			t.Error("PB 与 T3 应关联")
		}
		if !slices.Contains(k.messages(t3), "prh 会议纪要") {
			t.Errorf("prh-att-3 应挂到 T3: %v", k.messages(t3))
		}
		eq(t, "T3 进展数", len(k.progress(t3)), prog0+1)
		evs := k.attachedEvents(t3, pb)
		if len(evs) != 1 {
			t.Fatalf("应有一条 proposal.attached: %d", len(evs))
		}
		eq(t, "操作者", evs[0].Actor.Type, "human")
		eq(t, "via", evs[0].Actor.Via, "web")
	})

	k.step("ACT-PRH-11 补到的目标不合法时被拒绝且建议不变", func(t *testing.T) {
		k.draftDefault("prh-att-4")
		k.emit(srcA, Ev{ID: "prh-att-4", Subject: "prh-att-4"})
		pc = k.proposalFrom(srcA, "prh-att-4")
		doneItem := k.openItem("prh 已终结事项")
		k.apa(k.DEL, 0, "item", "transition", doneItem, "--to", "done", "--reason", "做完了", "--evidence", "做完了")
		anotherProposed := k.proposal("prh 另一条建议")
		before := len(k.attachedEvents(pc, pc)) + len(k.events("", "proposal.attached"))
		if r := k.Web("POST", "/v1/items/"+pc+"/attach", `{"to":"`+doneItem+`"}`); r.Code != 409 {
			t.Errorf("目标已终结应返回 409: %d %s", r.Code, r.Body)
		}
		if r := k.Web("POST", "/v1/items/"+pc+"/attach", `{"to":"itm-not-exist"}`); r.Code != 404 {
			t.Errorf("目标不存在应返回 404: %d %s", r.Code, r.Body)
		}
		if r := k.Web("POST", "/v1/items/"+pc+"/attach", `{"to":"`+anotherProposed+`"}`); r.Code < 400 || r.Code >= 500 {
			t.Errorf("目标在 proposed 应被拒绝（4xx）: %d %s", r.Code, r.Body)
		}
		eq(t, "PC 阶段", k.phase(pc), "proposed")
		ev, _ := k.srcEvent(srcA, "prh-att-4")
		eq(t, "prh-att-4 仍未挂靠", ev.Disposition, "proposed")
		eq(t, "prh-att-4 的事项", ev.Item, pc)
		eq(t, "proposal.attached 数", len(k.attachedEvents(pc, pc))+len(k.events("", "proposal.attached")), before)
		for _, e := range k.events("", "proposal.attached") {
			if e.Payload["proposal"] == pc {
				t.Errorf("PC 不应有 proposal.attached: %+v", e)
			}
		}
	})

	k.step("ACT-PRH-12 准备批量处理用的四条建议", func(t *testing.T) {
		// 批量按推荐会处理范围内所有带推荐的建议。前面各步留下的建议属于其他动作，先由本人在 Web 处理掉，
		// 让"按推荐处理"的范围只剩 B1–B4（验收文档的 S0 之后"共三条"以此为前提）。
		for id := range k.stream() {
			k.webOK("POST", "/items/"+id+"/dismiss", `{"reason":"批量前清场"}`, 200)
		}
		eq(t, "清场后的建议流", len(k.stream()), 0)
		k.draft("prh-b-acc", map[string]any{"title": "起草：prh-b-acc", "kind": "task", "recommend": "accept", "reason": "需要跟进"})
		k.draft("prh-b-dis", map[string]any{"title": "起草：prh-b-dis", "kind": "task", "recommend": "dismiss", "reason": "广告"})
		k.draft("prh-b-att", map[string]any{"title": "起草：prh-b-att", "kind": "task", "recommend": "attach", "attach_to": t3, "reason": "属于合同谈判"})
		for _, id := range []string{"prh-b-acc", "prh-b-dis", "prh-b-att"} {
			k.emit(srcA, Ev{ID: id, Subject: id})
		}
		b[1], b[2], b[3] = k.proposalFrom(srcA, "prh-b-acc"), k.proposalFrom(srcA, "prh-b-dis"), k.proposalFrom(srcA, "prh-b-att")
		b[4] = k.idOf(k.apa(k.OBS, 0, "propose", "--kind", "task", "--title", "prh-b-none", "--project", k.Projects["acc-open"]))
		for i, rec := range []string{"accept", "dismiss", "attach"} {
			eq(t, "B"+itoa(int64(i+1))+" 的推荐", k.proposalOf(b[i+1])["recommend"], rec)
		}
		if k.proposalOf(b[4]) != nil {
			t.Errorf("B4 不应有 proposal: %v", k.proposalOf(b[4]))
		}
		s0 = k.maxSeq()
	})

	k.step("ACT-PRH-13 本人在 Web 一次点击按推荐批量处理", func(t *testing.T) {
		body := `{"mode":"recommended","filter":{"source":"` + srcA + `","project":"` + k.Projects["acc-open"] + `"}}`
		res := parseResults(t, k.webOK("POST", "/proposals/batch", body, 200))
		for _, id := range b[1:4] {
			if !res[id].OK {
				t.Errorf("%s 应处理成功: %+v", id, res[id])
			}
		}
		if _, ok := res[b[4]]; ok {
			t.Errorf("没有推荐的 B4 不应被处理: %+v", res[b[4]])
		}
		eq(t, "B1", k.phase(b[1]), "open")
		eq(t, "B2", k.phase(b[2]), "dropped")
		eq(t, "B2 reason", k.last(b[2], "item.phase_changed").Reason, "广告")
		eq(t, "B3", k.phase(b[3]), "dropped")
		eq(t, "B3 补到 T3 的 proposal.attached", len(k.attachedEvents(t3, b[3])), 1)
		eq(t, "B4", k.phase(b[4]), "proposed")
		objs := map[string]int{}
		for _, e := range k.disposals(s0) {
			if e.Type == "item.phase_changed" {
				objs[e.Object]++
				eq(t, "操作者", e.Actor.Type, "human")
				eq(t, "via", e.Actor.Via, "web")
			}
		}
		if len(objs) != 3 || objs[b[1]] != 1 || objs[b[2]] != 1 || objs[b[3]] != 1 {
			t.Errorf("S0 之后应对 B1、B2、B3 各有一条处置 Event（共三条）: %v", objs)
		}
	})

	k.step("ACT-PRH-14 按推荐批量时目标已关闭的那一条跳过并说明，其余照常", func(t *testing.T) {
		t4 := k.openItem("prh 将关闭的事项")
		k.draft("prh-skip-1", map[string]any{"title": "起草：prh-skip-1", "kind": "task", "recommend": "attach", "attach_to": t4, "reason": "属于 T4"})
		k.draft("prh-skip-2", map[string]any{"title": "起草：prh-skip-2", "kind": "task", "recommend": "accept", "reason": "需要跟进"})
		k.emit(srcA, Ev{ID: "prh-skip-1", Subject: "prh-skip-1"})
		k.emit(srcA, Ev{ID: "prh-skip-2", Subject: "prh-skip-2"})
		s1, s2 := k.proposalFrom(srcA, "prh-skip-1"), k.proposalFrom(srcA, "prh-skip-2")
		eq(t, "prh-skip-1 的推荐目标", k.proposalOf(s1)["attach_to"], t4)
		k.apa(k.DEL, 0, "item", "transition", t4, "--to", "done", "--reason", "做完了", "--evidence", "做完了")
		res := parseResults(t, k.webOK("POST", "/proposals/batch", `{"mode":"recommended","filter":{"ids":["`+s1+`","`+s2+`"]}}`, 200))
		if res[s1].OK || res[s1].Why == "" {
			t.Errorf("prh-skip-1 应失败并给出 why: %+v", res[s1])
		}
		if !res[s2].OK {
			t.Errorf("prh-skip-2 应成功: %+v", res[s2])
		}
		eq(t, "prh-skip-1", k.phase(s1), "proposed")
		eq(t, "prh-skip-2", k.phase(s2), "open")
		if len(k.events(s2, "item.phase_changed")) == 0 {
			t.Error("成功的那条应有自己的 Event")
		}
	})

	k.step("ACT-PRH-15 delegate 用处置清单批量处理，每条各自成败", func(t *testing.T) {
		for _, n := range []string{"prh-e-1", "prh-e-2", "prh-e-3"} {
			k.draft(n, map[string]any{"title": "起草：" + n, "kind": "task", "recommend": "accept", "reason": "需要跟进"})
		}
		k.emit(srcB, Ev{ID: "prh-e-1", Subject: "prh-e-1"})
		k.emit(srcB, Ev{ID: "prh-e-2", Subject: "prh-e-2"})
		k.emit(srcB, Ev{ID: "prh-e-3", Subject: "prh-e-3"})
		e1, e2, e3 = k.proposalFrom(srcB, "prh-e-1"), k.proposalFrom(srcB, "prh-e-2"), k.proposalFrom(srcB, "prh-e-3")
		plan, _ := json.Marshal(map[string]any{"actions": []map[string]any{
			{"id": e1, "do": "accept"}, {"id": e2, "do": "dismiss", "reason": "不需要"}, {"id": e3, "do": "attach", "to": t3}, {"id": "itm-not-exist", "do": "accept"}}})
		file := filepath.Join(k.Root, "plan.json")
		k.must(os.WriteFile(file, plan, 0o644))
		s := k.maxSeq()
		r := k.apa(k.DEL, 0, "proposals", "apply", "--file", file, "--evidence", "这几条按我说的处理")
		res := parseResults(t, r.Stdout)
		for _, id := range []string{e1, e2, e3} {
			if !res[id].OK {
				t.Errorf("%s 应成功: %+v", id, res[id])
			}
		}
		if bad := res["itm-not-exist"]; bad.OK || bad.Code == "" || bad.Why == "" {
			t.Errorf("itm-not-exist 应失败并给出错误码与原因: %+v", bad)
		}
		eq(t, "E1", k.phase(e1), "open")
		eq(t, "E2", k.phase(e2), "dropped")
		eq(t, "E2 reason", k.last(e2, "item.phase_changed").Reason, "不需要")
		eq(t, "E3", k.phase(e3), "dropped")
		eq(t, "E3 补到 T3", len(k.attachedEvents(t3, e3)), 1)
		got := map[string]bool{}
		for _, e := range k.disposals(s) {
			prop := e.Object
			if e.Type == "proposal.attached" {
				prop, _ = e.Payload["proposal"].(string)
			}
			if !slices.Contains([]string{e1, e2, e3}, prop) {
				continue
			}
			if e.Type == "item.phase_changed" || e.Type == "proposal.attached" {
				got[prop] = true
				eq(t, "on_behalf_of", e.Actor.OnBehalfOf, "owner")
				eq(t, "证据", e.Ev, "这几条按我说的处理")
				eq(t, "会话类型", e.Actor.SessionKind, "direct")
			}
		}
		eq(t, "有处置 Event 的成功条数", len(got), 3)
	})

	k.step("ACT-PRH-16 批量处理的结果可以逐条撤销", func(t *testing.T) {
		var mine []step
		for _, e := range k.agentChanges() {
			if e.Type == "item.phase_changed" && slices.Contains([]string{e1, e2, e3}, e.Object) {
				mine = append(mine, e)
			}
		}
		eq(t, "agent 变更视图里的三条处置", len(mine), 3)
		var dismissSeq int64
		for _, e := range mine {
			if e.Object == e2 {
				dismissSeq = e.Seq
			}
		}
		before := len(k.events(e2, ""))
		k.webOK("POST", "/items/"+e2+"/revert", `{"event":`+itoa(dismissSeq)+`}`, 200)
		eq(t, "E2 阶段", k.phase(e2), "proposed")
		if len(k.events(e2, "")) <= before {
			t.Error("时间线应多一条撤销 Event")
		}
		eq(t, "原 Event 仍在", k.events(e2, "item.phase_changed")[len(k.events(e2, "item.phase_changed"))-2].Seq, dismissSeq)
		eq(t, "E1", k.phase(e1), "open")
		eq(t, "E3", k.phase(e3), "dropped")
	})

	k.step("ACT-PRH-17 批量处理缺依据或超过上限时整批不生效", func(t *testing.T) {
		before := k.stream()
		disposals := len(k.disposals(0))
		k.apa(k.DEL, exitBasis, "proposals", "apply", "--recommended", "--source", srcA)
		var acts []map[string]any
		for i := 0; i < 201; i++ {
			acts = append(acts, map[string]any{"id": b[4], "do": "accept"})
		}
		body, _ := json.Marshal(map[string]any{"mode": "explicit", "actions": acts})
		r := k.Web("POST", "/v1/proposals/batch", string(body))
		if r.Code < 400 || r.Code >= 500 || !strings.Contains(r.Body, "200") {
			t.Errorf("201 条应被整批拒绝（4xx，说明一次至多 200 条）: %d %s", r.Code, r.Body)
		}
		after := k.stream()
		eq(t, "建议流条数", len(after), len(before))
		for id := range before {
			if _, ok := after[id]; !ok {
				t.Errorf("建议 %s 不应被处理", id)
			}
		}
		eq(t, "B4", k.phase(b[4]), "proposed")
		eq(t, "新增处置 Event", len(k.disposals(0)), disposals)
	})

	k.step("ACT-PRH-18 事件订阅能取到新的稳定事件，随包订阅方跳过不处理的类型", func(t *testing.T) {
		k.apa(k.DEL, 0, "item", "progress", t3, "--text", "prh 已发回意见", "--evidence", "记一下进展")
		types := map[string]bool{}
		var progressSeq int64
		for _, e := range k.feedLines(k.OBS, "events", "--cursor", c0) {
			types[e.Type] = true
			if e.Type == "item.progress_added" {
				progressSeq = max(progressSeq, e.Seq)
			}
		}
		if !types["proposal.attached"] || !types["item.progress_added"] {
			t.Errorf("事件流应含 proposal.attached 与 item.progress_added: %v", types)
		}
		k.forwardOnlyCreated(progressSeq)
	})
}
