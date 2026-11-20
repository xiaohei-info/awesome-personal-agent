//go:build e2e

package extractalign

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/srcstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// accWBPack 是用户能力包 acc-wb（ACT-XAL-01，附录 C.5）。
const accWBPack = `name: acc-wb
version: 1.0.0
description: 抽取对齐与紧急提醒验收包
source_rules:
  - {id: chat-extract, match: {source_type: chat, event_type: [chat.message]}, then: extract, project_by: draft}
  - {id: raw-prop, match: {source_type: acc-raw, event_type: [acc.raw]}, then: proposed, kind: task}
  - {id: chat-np, match: {source_type: chat-np, event_type: [chat.message]}, then: extract, prescreen: none, project_by: draft}
`

// writePack 重写 acc-wb 包目录：pack.yaml 与 extra（相对路径 → 内容）。
func (f *fx) writePack(yaml string, extra ...map[string]string) {
	f.t.Helper()
	dir := filepath.Join(f.Home, "packs", "acc-wb")
	f.must(os.RemoveAll(dir))
	f.must(os.MkdirAll(dir, 0o755))
	f.must(os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(yaml), 0o644))
	for _, m := range extra {
		for rel, body := range m {
			f.must(os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755))
			f.must(os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644))
		}
	}
}

func (f *fx) restart() {
	f.t.Helper()
	f.StopKernel()
	f.StartKernelAgain()
	f.WebLogin()
}

// chat 取 SRC_STUB 的 chat-two-things 事件，换成唯一的事件 id 与关联键（不挂靠已有事项）。
func chat(id string) map[string]any {
	// 内核对完全相同的输入复用上次的生成结果（direct 的指纹），所以每条事件的标题不同。
	return chatSubject(id, "Team chat "+id)
}

func chatSubject(id, subject string) map[string]any {
	evs, _ := srcstub.Events("chat-two-things", nil)
	e := evs[0]
	e["id"] = id
	e["correlation_keys"] = []string{"room-" + id}
	if subject != "" {
		e["subject"] = subject
	}
	return e
}

// genReq 是生成端点收到的一次起草或抽取请求：结构化输入与资料段。
type genReq struct {
	Extract  bool
	Input    map[string]any
	Material string
	Raw      string
}

// genReqs 取生成端点里 source 来源的起草/抽取请求（按到达顺序）。
func (f *fx) genReqs(source string) []genReq {
	var out []genReq
	for _, r := range f.ModelStub.Requests() {
		var b struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if json.Unmarshal(r.Body, &b) != nil {
			continue
		}
		for _, m := range b.Messages {
			if m.Role != "user" || !strings.HasPrefix(m.Content, "## 结构化输入\n") {
				continue
			}
			in, mat, _ := strings.Cut(strings.TrimPrefix(m.Content, "## 结构化输入\n"), "\n\n## 资料")
			var inp map[string]any
			if json.Unmarshal([]byte(in), &inp) != nil || inp["source"] != source {
				continue
			}
			out = append(out, genReq{Extract: strings.Contains(string(r.Body), `"candidates"`) && strings.Contains(string(r.Body), `"refs"`), Input: inp, Material: mat, Raw: string(r.Body)})
		}
	}
	return out
}

func ids(l any) []string {
	var out []string
	for _, x := range l.([]any) {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

func (f *fx) setLocality(loc string) {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{
		"judge_endpoint":    map[string]any{"url": f.Cls.URL(), "api_key": "stub-key", "protocol": modelstub.ProtocolJudgment, "locality": "local"},
		"generate_endpoint": map[string]any{"url": f.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": loc}})
	f.webOK("PUT", "/settings/model-endpoints", string(b), 200)
}

func (f *fx) proposedTitled(title string) []string {
	f.t.Helper()
	var out struct {
		Items []struct{ ID, Title string } `json:"items"`
	}
	jsonOf(f.t, f.webOK("GET", "/items?phase=proposed&limit=500", "", 200), &out)
	var r []string
	for _, it := range out.Items {
		if it.Title == title {
			r = append(r, it.ID)
		}
	}
	return r
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// newFx 建好替身环境并接好两个模型端点。
func newFx(t *testing.T) *fx {
	t.Helper()
	f := &fx{Env: baseline.Setup(t), t: t}
	f.WireEndpoints()
	return f
}

func TestFlowExtractAlign(t *testing.T) {
	f := newFx(t)
	f.Cls.SetDefaultPYes(0.95) // capture-prescreen p_yes=0.95
	var x1 string
	var chatSrc, npSrc *source
	var evA string
	var itemA, itemB string

	f.step("ACT-XAL-01 准备会话来源与用户能力包规则", func() {
		f.writePack(accWBPack)
		f.restart()
		chatSrc = f.register("src_accchat", "chat")
		npSrc = f.register("src_accchatnp", "chat-np")
		if c := f.packsCheck(); c.Status != "ok" || c.Data.Rejected != 0 {
			t.Fatalf("acc-wb 应加载无拒绝: %+v", c)
		}
		x1 = f.webItemID("发布 v2 文档")
	})

	f.step("ACT-XAL-02 一条含两件事的事件抽出两条候选并各归项目", func() {
		f.draftWB("two-refs", modelstub.WBParams{ProjectA: f.Projects["acc-work"], ProjectB: f.Projects["acc-home"], AttachTo: x1})
		evA = "xal-a"
		e := chatSrc.emit(chat(evA))[evA]
		eq(t, "disposition", e.Disposition, "proposed")
		if len(e.Items) != 2 || e.Item != e.Items[0] {
			t.Fatalf("items=%v item=%s，期望两条且 item 为第一条", e.Items, e.Item)
		}
		itemA, itemB = e.Items[0], e.Items[1]
		a, b := f.webItem(itemA), f.webItem(itemB)
		eq(t, "A 项目", a["project"], f.Projects["acc-work"])
		eq(t, "A 推荐", f.proposal(itemA)["recommend"], "attach")
		eq(t, "A attach_to", f.proposal(itemA)["attach_to"], x1)
		eq(t, "B 项目", b["project"], f.Projects["acc-home"])
		eq(t, "A 正文是 messages[0]、[1]", a["body"], "alice: Can you publish the v2 docs by Friday?\nbob: Sure, I will draft the changelog first.")
		eq(t, "B 正文是 messages[2]", b["body"], "alice: Also the invoice for the venue needs paying this week.")
	})

	f.step("ACT-XAL-03 三步共用同一份清单，且清单有界、按外发范围过滤", func() {
		// 预筛（分类端点）与抽取（生成端点）看到同一份清单。
		var pre string
		for _, b := range f.Cls.Batches() {
			if strings.Contains(b.BodyText, `"source":"src_accchat"`) && strings.Contains(b.BodyText, "未关闭事项") {
				pre = b.BodyText
			}
		}
		if pre == "" || !strings.Contains(pre, x1) || !strings.Contains(pre, "发布 v2 文档") {
			t.Fatalf("预筛资料没有带未关闭事项清单（含 X1）: %s", pre)
		}
		first := f.genReqs("src_accchat")
		if len(first) == 0 || !first[0].Extract {
			t.Fatalf("没有抽取请求: %+v", first)
		}
		eq(t, "抽取清单与预筛清单同一份（X1）", strings.Join(ids(first[0].Input["open_items"]), ","), x1)
		if p, _ := first[0].Input["projects"].([]any); len(p) == 0 {
			t.Error("抽取输入没有活动项目清单")
		}

		// 来自 acc-raw-none 的 open 事项：外发范围 none，且清单 130+ 件。
		raw := f.register("src_accraw", "acc-raw")
		f.webOK("PUT", "/settings/sources/src_accraw/outbound", `{"local":"full","external":"none"}`, 200)
		rawEv := raw.emit(map[string]any{"id": "xal-raw", "type": "acc.raw", "subject": "RAWNONE-机密", "summary": "来自外发 none 的来源",
			"occurred_at": "2026-11-10T09:00:00Z"})["xal-raw"]
		if rawEv.Item == "" {
			t.Fatalf("raw 来源没有产生事项: %+v", rawEv)
		}
		f.webOK("POST", "/items/"+rawEv.Item+"/transition", `{"to":"open","reason":"接受"}`, 200)
		raw.setCase("")
		for i := 0; i < 130; i++ {
			f.webItemID(fmt.Sprintf("FILL-%03d", i))
		}
		f.setLocality("external")
		before := len(f.genReqs("src_accchat"))
		ev := chatSrc.emit(chatSubject("xal-c", "发布 v2 文档"))["xal-c"]
		eq(t, "xal-c disposition", ev.Disposition, "proposed")
		reqs := f.genReqs("src_accchat")
		var ext genReq
		for _, r := range reqs[before:] {
			if r.Extract {
				ext = r
			}
		}
		if ext.Input == nil {
			t.Fatal("外部端点没有收到抽取请求")
		}
		open := ids(ext.Input["open_items"])
		if len(open) > 100 {
			t.Errorf("open_items %d 条，至多 100", len(open))
		}
		if total, _ := ext.Input["open_items_total"].(float64); total < 131 {
			t.Errorf("open_items_total = %v，应写明总数（≥131）", ext.Input["open_items_total"])
		}
		if open[0] != x1 {
			t.Errorf("X1 应排在清单最前，实际首项 %s", open[0])
		}
		if strings.Contains(ext.Raw, "RAWNONE") || strings.Contains(ext.Raw, rawEv.Item) {
			t.Error("外部端点的清单里出现了出自外发范围 none 来源的事项")
		}
		f.WireEndpoints() // 生成端点改回 local
	})

	f.step("ACT-XAL-04 清单外的目标与项目被丢弃，候选保留", func() {
		f.draftWB("bad-target", modelstub.WBParams{})
		e := chatSrc.emit(chat("xal-d"))["xal-d"]
		eq(t, "disposition", e.Disposition, "proposed")
		if len(e.Items) != 1 {
			t.Fatalf("items=%v，候选应保留", e.Items)
		}
		it := f.webItem(e.Item)
		eq(t, "title", it["title"], "补充合同条款")
		p := f.proposal(e.Item)
		if p["recommend"] != nil || p["attach_to"] != nil {
			t.Errorf("清单外目标：推荐应作废，实际 %v", p)
		}
		if pr := str(it, "project"); pr != "" {
			t.Errorf("清单外项目应丢弃，实际 %q", pr)
		}
	})

	f.step("ACT-XAL-05 同一件事第二次抽出时并入已有建议", func() {
		f.TCAdvance(86400)
		f.draftWB("two-refs", modelstub.WBParams{ProjectA: f.Projects["acc-work"], ProjectB: f.Projects["acc-home"], AttachTo: x1})
		n := len(f.proposedTitled("把合同发给小王"))
		e := chatSrc.emit(chat("xal-e"))["xal-e"]
		if got := len(f.proposedTitled("把合同发给小王")); got != n || got != 1 {
			t.Errorf("同标题建议数 %d（之前 %d），期望仍为 1", got, n)
		}
		eq(t, "事件的 item 指向 A", e.Item, itemA)
		var src []string
		for _, s := range f.proposal(itemA)["sources"].([]any) {
			src = append(src, s.(string))
		}
		if !strings.Contains(strings.Join(src, ","), e.ID) {
			t.Errorf("A 的 proposal.sources %v 没有并入事件 %s", src, e.ID)
		}
	})

	f.step("ACT-XAL-06 prescreen none 跳过预筛且不产生预筛判断记录", func() {
		f.draftWB("two-refs", modelstub.WBParams{ProjectA: f.Projects["acc-work"], ProjectB: f.Projects["acc-home"], AttachTo: x1})
		e := npSrc.emit(chat("xal-np"))["xal-np"]
		eq(t, "disposition", e.Disposition, "proposed")
		if len(e.Items) != 2 {
			t.Fatalf("抽取照常产出建议，实际 items=%v", e.Items)
		}
		for _, b := range f.Cls.Batches() {
			if strings.Contains(b.BodyText, `"source":"src_accchatnp"`) {
				t.Errorf("分类端点不应收到 chat-np 的预筛请求: %s", b.BodyText)
			}
		}
		for _, r := range f.ModelStub.Requests() {
			if strings.Contains(r.BodyText, "src_accchatnp") && strings.Contains(r.BodyText, "有没有需要记下的事") {
				t.Error("生成端点不应收到 chat-np 的预筛请求")
			}
		}
		if e.Judgment != "" {
			if j := f.webOK("GET", "/judgments/"+e.Judgment, "", 200); strings.Contains(j, "capture-prescreen") {
				t.Errorf("判断记录里不应有预筛一级: %s", j)
			}
		}
		// 对话增量始终预筛。
		f.webOK("PATCH", "/projects/"+f.Projects["acc-open"], `{"capture":true}`, 200)
		n := 0
		for _, b := range f.Cls.Batches() {
			if strings.Contains(b.BodyText, "有没有需要记下的事") {
				n++
			}
		}
		body, _ := json.Marshal(map[string]string{"text": "我: 周三前要把预算表发给财务", "project": f.Projects["acc-open"]})
		req, _ := http.NewRequest("POST", f.URL+"/v1/capture", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.WRK)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-APA-Session", "xal-cap")
		resp, err := http.DefaultClient.Do(req)
		f.must(err)
		resp.Body.Close()
		eq(t, "capture HTTP", resp.StatusCode, 200)
		f.until("对话增量触发预筛请求", func() bool {
			m := 0
			for _, b := range f.Cls.Batches() {
				if strings.Contains(b.BodyText, "有没有需要记下的事") {
					m++
				}
			}
			return m > n
		})
	})

	f.step("ACT-XAL-07 旧的三字段输出仍被接受", func() {
		f.draftWB("legacy3", modelstub.WBParams{})
		e := chatSrc.emit(chat("xal-g"))["xal-g"]
		eq(t, "disposition", e.Disposition, "proposed")
		it := f.webItem(e.Item)
		eq(t, "title", it["title"], "旧输出的候选")
		eq(t, "kind", it["kind"], "task")
		eq(t, "summary", it["summary"], "只有三个字段")
		p := f.proposal(e.Item)
		if p["recommend"] != nil || p["urgent"] != nil {
			t.Errorf("旧输出不应带推荐与 urgent: %v", p)
		}
	})

	f.step("ACT-XAL-08 一条事件至多取 20 条候选", func() {
		f.draftWB("cands-25", modelstub.WBParams{})
		e := chatSrc.emit(chat("xal-h"))["xal-h"]
		eq(t, "items 数", len(e.Items), 20)
		n := 0
		for i := 1; i <= 25; i++ {
			n += len(f.proposedTitled(fmt.Sprintf("候选 %02d", i)))
		}
		eq(t, "建议数", n, 20)
	})

	f.step("ACT-XAL-09 能力包加载拒绝误用", func() {
		judge := "---\nagent: judge\nquestion_type: yes_no\non_undecided: yes\n---\n是吗\n"
		head := "name: acc-wb\nversion: 1.0.0\ndescription: 误用\nsource_rules:\n"
		cases := []struct {
			name, field string
			yaml        string
			extra       map[string]string
		}{
			{"模板名 none", "none", head + "  - {id: ok, match: {source_type: chat}, then: extract}\n", map[string]string{"prompts/none.md": judge}},
			{"screen 配 prescreen none", "prescreen", head + "  - {id: bad, match: {source_type: chat}, then: screen, prescreen: none}\n", nil},
			{"proposed 配 notify_urgent", "notify_urgent", head + "  - {id: bad, match: {source_type: chat}, then: proposed, notify_urgent: true}\n", nil},
		}
		for _, c := range cases {
			f.writePack(c.yaml, c.extra)
			f.restart()
			chk := f.packsCheck()
			if chk.Status == "ok" || chk.Data.Rejected != 1 || !strings.Contains(fmt.Sprint(chk.Details), "acc-wb") || !strings.Contains(chk.Why+fmt.Sprint(chk.Details), c.field) {
				t.Errorf("%s：acc-wb 应被整包拒绝且原因指出 %s，实际 %+v", c.name, c.field, chk)
			}
		}
		f.writePack(accWBPack) // 验完撤销
		f.restart()
		if c := f.packsCheck(); c.Status != "ok" || c.Data.Rejected != 0 {
			t.Errorf("撤销后 acc-wb 应恢复加载: %+v", c)
		}
	})
	_ = itemB
}
