//go:build e2e

package materialbudget

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	genBudget       = 3000 // ACT-BUD-02 起生成端点的资料预算
	genBudgetTokens = 2000 // 同一份预算折成 token（3000 字 ÷ 1.5），页面与接口按 token 存
	judgeBudget     = 8000 // 判断端点的资料预算缺省
	excerptLen      = 500  // excerpt 外发范围的字符数
)

type mat struct {
	Sent     int    `json:"sent_chars"`
	Original int    `json:"original_chars"`
	Mode     string `json:"mode"`
}

// headTailCut 把"送给模型的资料"按首 3/4 尾 1/4 的规则拆开校验：head 是原文开头，marker 写明省略数，tail 是原文结尾。
// 返回送出的字符数（head + 省略行 + tail）。
func headTailCut(t *testing.T, req, orig string, budget int) (sent int) {
	t.Helper()
	n := runes(orig)
	start := strings.Index(req, orig[:60])
	if start < 0 {
		t.Fatal("起草请求里找不到原文开头")
	}
	loc := regexp.MustCompile(`\n\[省略 (\d+) 个字符\]\n`).FindStringSubmatchIndex(req[start:])
	if loc == nil {
		t.Fatalf("起草请求里没有\"省略 N 个字符\"一行")
	}
	omitted, _ := strconv.Atoi(req[start+loc[2] : start+loc[3]])
	markerEnd := start + loc[1]
	head := req[start : start+loc[0]]
	keep := n - omitted
	tailLen := keep - runes(head)
	tail := req[markerEnd : markerEnd+tailLen]
	if head != orig[:len(head)] {
		t.Error("资料开头不是原文开头")
	}
	if tail != orig[n-tailLen:] {
		t.Error("资料结尾不是原文结尾")
	}
	if rest := req[markerEnd+tailLen:]; !strings.HasPrefix(rest, "\n") {
		t.Errorf("资料结尾之后紧跟的不是换行，原文尾部之外还有内容: %.40q", rest)
	}
	if h, ex := runes(head), keep*3/4; h != ex {
		t.Errorf("开头 %d 个字符，应占保留字符 %d 的 3/4 = %d", h, keep, ex)
	}
	sent = runes(head) + runes(req[start+loc[0]:markerEnd]) + tailLen
	if sent > budget {
		t.Errorf("资料 %d 个字符，超出预算 %d", sent, budget)
	}
	if h := runes(head); h < budget*3/4-40 || h > budget*3/4 {
		t.Errorf("开头约 %d 个字符，期望约 %d", h, budget*3/4)
	}
	if tl := tailLen; tl < budget/4-40 || tl > budget/4 {
		t.Errorf("结尾约 %d 个字符，期望约 %d", tl, budget/4)
	}
	return sent
}

func TestFlowMaterialBudget(t *testing.T) {
	k := setup(t)
	bigBody := strings.TrimSpace(asString(k.stubOut("big-body")[0]["detail"]))
	bigRaw := asString(k.stubOut("big-body")[0]["detail"])
	var bigEv srcEvent
	var bigItem string
	var bigSent int

	k.step("ACT-BUD-01 生成端点的资料预算缺省 16000 token（原 24000 字）且只在 Web 可改", func(t *testing.T) {
		_, ep := k.endpoints("")
		eq(t, "未设过时 generate_endpoint.max_material_tokens（与旧 24000 字等价）", ep.Generate.MaxMaterialTokens, 16000)
		k.putEndpoints("local", genBudgetTokens)
		_, ep = k.endpoints("")
		eq(t, "保存后读回 max_material_tokens", ep.Generate.MaxMaterialTokens, genBudgetTokens)
		eq(t, "判断端点的资料预算缺省", ep.Judge.MaxMaterialChars, judgeBudget)
		if n := len(k.audit("policy.changed")); n == 0 {
			t.Error("保存资料预算应记 policy.changed")
		}
		r := k.APIRaw("PUT", "/v1/settings/model-endpoints", `{"generate_endpoint":{"max_material_tokens":2000}}`, k.DEL)
		eq(t, "DEL 调 PUT /v1/settings/model-endpoints 的 HTTP 状态", r.Code, 403)
		_, ep = k.endpoints("")
		eq(t, "被拒后预算不变", ep.Generate.MaxMaterialTokens, genBudgetTokens)
	})

	k.step("ACT-BUD-02 超预算的长正文按首 3/4 尾 1/4 裁剪送给起草", func(t *testing.T) {
		k.addStub("src_acc_raw", "mail", "full", "full")
		k.screenYes("Big body", 0.95)
		k.draftReply("Big body", map[string]any{"title": "预算验收：长邮件", "kind": "task", "summary": "长正文"})
		k.clear()
		k.setCase("src_acc_raw", "big-body")
		bigEv = k.disposed("src_acc_raw", "big-big-body")
		eq(t, "处置", bigEv.Disposition, "proposed")
		bigItem = bigEv.Item
		req := k.waitGenerate("Big body")
		sent := headTailCut(t, req, bigBody, genBudget)
		bigSent = sent
		t.Logf("起草请求里的资料 %d / %d 字符", sent, runes(bigBody))
	})

	k.step("ACT-BUD-03 聚合消息从最新一条往前整条保留", func(t *testing.T) {
		k.loadPack("acc-bud", `name: acc-bud
version: 1.0.0
description: 资料预算验收包：会话来源抽取；来源对象的更新刷新事项（规则没有 on_update，ACT-BUD-08）
source_rules:
  - {id: chat-extract, match: {source_type: chat, event_type: [chat.message]}, then: extract}
  - {id: obj-proposed, match: {source_type: acc-obj, event_type: [mail.received]}, kind: task, project_by: none, then: proposed}
`)
		k.addStub("src_acc_chat", "chat", "full", "full")
		k.screenYes("Long chat", 0.95)
		r, err := modelstub.WBCase("legacy3", modelstub.WBParams{})
		if err != nil {
			t.Fatal(err)
		}
		k.ModelStub.When("Long chat", r)
		k.clear()
		k.setCase("src_acc_chat", "chat-long")
		k.disposed("src_acc_chat", "chat-long-1")
		req := k.waitGenerate("Long chat")
		items := k.stubOut("chat-long")[0]["items"].([]any)
		total := len(items)
		var want []string // 每条完整消息送出时的样子：发送者: 正文
		for _, it := range items {
			m := it.(map[string]any)
			want = append(want, asString(m["sender"])+": "+asString(m["text"]))
		}
		loc := regexp.MustCompile(`\[省略更早的 (\d+) 条消息\]\n`).FindStringSubmatchIndex(req)
		if loc == nil {
			t.Fatalf("抽取请求里没有\"省略更早的 N 条消息\"一行:\n%s", req)
		}
		omitted, _ := strconv.Atoi(req[loc[2]:loc[3]])
		var kept []string
		for _, ln := range strings.Split(req[loc[1]:], "\n") {
			if ln == "" {
				break
			}
			kept = append(kept, ln)
		}
		if len(kept) == 0 || omitted == 0 {
			t.Fatalf("应保留最新的若干条并省略更早的: 保留 %d 条，省略 %d 条", len(kept), omitted)
		}
		eq(t, "省略条数 + 保留条数", omitted+len(kept), total)
		for i, ln := range kept {
			if ln != want[omitted+i] {
				t.Errorf("保留的第 %d 条不是第 %d 条消息的完整原样（被切开，或不是最新的）: %.60q", i, omitted+i, ln)
			}
		}
		eq(t, "最后一条保留的消息", kept[len(kept)-1], want[total-1])
		t.Logf("60 条消息：保留最新 %d 条，省略更早的 %d 条", len(kept), omitted)
	})

	k.step("ACT-BUD-04 裁剪量在判断记录、处置载荷与处理经过里可见，原文不变", func(t *testing.T) {
		// 判断记录：apa why 里初筛一级记送出字符数、裁剪前字符数与方式（判断端点预算 8000）。
		var why struct {
			Steps []struct {
				Judgment *struct {
					Record struct {
						Levels []struct {
							Material *mat `json:"material"`
						} `json:"levels"`
					} `json:"record"`
				} `json:"judgment"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(k.apa(k.OBS, 0, "why", bigItem).Stdout), &why); err != nil {
			t.Fatal(err)
		}
		var lv *mat
		for _, s := range why.Steps {
			if s.Judgment != nil && len(s.Judgment.Record.Levels) > 0 {
				lv = s.Judgment.Record.Levels[0].Material
			}
		}
		if lv == nil {
			t.Fatal("apa why 的判断记录初筛一级没有 material")
		}
		eq(t, "初筛送出字符数（判断端点预算）", lv.Sent, judgeBudget)
		eq(t, "初筛裁剪前字符数", lv.Original, runes(bigBody))
		eq(t, "初筛裁剪方式", lv.Mode, "head_tail")

		// 处置载荷：GET /v1/audit/events 里该事件的 inbound.disposed 带 material。
		disp := k.disposedPayload(bigEv.ID)
		if disp.Material == nil {
			t.Fatal("处置载荷没有 material")
		}
		eq(t, "载荷送出字符数等于起草请求里实际送出的", disp.Material.Sent, bigSent)
		eq(t, "载荷裁剪前字符数", disp.Material.Original, runes(bigBody))
		eq(t, "载荷裁剪方式", disp.Material.Mode, "head_tail")

		// 处理经过页读的接口 GET /source-events/{id}/trace：处置行带的 material 与载荷一致（页面据此显示"送给模型 N / M 字符"）。
		var tr struct {
			Entries []struct {
				Steps []struct {
					Type    string `json:"type"`
					Payload struct {
						Material *mat `json:"material"`
					} `json:"payload"`
				} `json:"steps"`
			} `json:"entries"`
		}
		if err := json.Unmarshal([]byte(k.web("GET", "/v1/source-events/"+bigEv.ID+"/trace", "", 200)), &tr); err != nil {
			t.Fatal(err)
		}
		var disposedShown *mat
		for _, e := range tr.Entries {
			for _, s := range e.Steps {
				if s.Type == "inbound.disposed" && s.Payload.Material != nil {
					disposedShown = s.Payload.Material
				}
			}
		}
		if disposedShown == nil || *disposedShown != *disp.Material {
			t.Errorf("处理经过里处置行的裁剪量 %+v 应与载荷 %+v 一致", disposedShown, disp.Material)
		}

		// 账本里的原文没变：body 的 sha256 与 SRC_STUB 输出相同。
		var body struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(k.web("GET", "/v1/source-events/"+bigEv.ID+"?part=body", "", 200)), &body); err != nil {
			t.Fatal(err)
		}
		eq(t, "body 的 sha256", sha(body.Body), sha(bigRaw))
		eq(t, "body 的字节数", len(body.Body), 204800)
	})

	k.step("ACT-BUD-05 旧处置不显示裁剪量", func(t *testing.T) {
		// 结构版本 7 夹具升级后的副本：另起一套内核，不动本流程其余步骤的环境。
		fx := &kit{Env: baseline.New(t), t: t}
		f := fx.LedgerFixture(7)
		fx.ServeFixture(f, map[string]string{"unjudged_rejudge_days": "0"})
		if len(f.M.Proposed) == 0 {
			t.Fatal("夹具没有升级前已处置的事件")
		}
		for _, id := range f.M.Proposed {
			// 处置 Event 载荷：升级前的处置没有 material，也不是被补成零值。
			var out struct {
				Events []struct {
					Payload map[string]json.RawMessage `json:"payload"`
				} `json:"events"`
			}
			if err := json.Unmarshal([]byte(fx.web("GET", "/v1/audit/events?limit=50&type=inbound.disposed&object="+id, "", 200)), &out); err != nil {
				t.Fatal(err)
			}
			if len(out.Events) == 0 {
				t.Fatalf("%s 没有 inbound.disposed 记录", id)
			}
			for _, e := range out.Events {
				if _, has := e.Payload["material"]; has {
					t.Errorf("%s: 升级前的处置载荷不应有 material: %s", id, e.Payload["material"])
				}
			}
			// 处理经过页读的接口：照常返回，且没有伪造的裁剪量（页面据此不显示"送给模型 N / M 字符"）。
			if tr := fx.web("GET", "/v1/source-events/"+id+"/trace", "", 200); strings.Contains(tr, `"material"`) || strings.Contains(tr, "sent_chars") {
				t.Errorf("%s: 升级前事件的处理经过里不应出现裁剪量", id)
			}
		}
	})

	k.step("ACT-BUD-06 外发范围先于预算生效", func(t *testing.T) {
		k.putEndpoints("external", genBudgetTokens)
		defer k.putEndpoints("local", genBudgetTokens) // 流程结束改回 local
		k.addStub("src_acc_raw_ex", "mail", "full", "excerpt")
		k.clear()
		k.setCase("src_acc_raw_ex", "big-body")
		ev := k.disposed("src_acc_raw_ex", "big-big-body")
		eq(t, "处置", ev.Disposition, "proposed")
		req := k.waitGenerate("Big body")
		if !strings.Contains(req, bigBody[:excerptLen]) {
			t.Error("外部端点应收到正文的前 500 个字符")
		}
		if strings.Contains(req, bigBody[excerptLen:excerptLen+60]) {
			t.Error("外部端点收到了超出 excerpt 范围的正文")
		}
		m := k.disposedPayload(ev.ID).Material
		if m == nil {
			t.Fatal("处置载荷没有 material")
		}
		if m.Sent > excerptLen || m.Original > excerptLen || m.Mode != "none" {
			t.Errorf("excerpt 范围先裁到 %d 字符以内，预算 %d 不再裁剪: material = %+v", excerptLen, genBudget, *m)
		}
	})

	k.step("ACT-BUD-07 建议事项正文超过 4000 字符时只写前 4000 并链到原文", func(t *testing.T) {
		long := k.item(bigItem)
		eq(t, "长事件事项正文", long.Body, proposalBody(bigRaw, bigEv.ID))
		if long.Proposal == nil || !contains(long.Proposal.Sources, bigEv.ID) {
			t.Errorf("proposal.sources[] 应含来源事件 %s: %+v", bigEv.ID, long.Proposal)
		}
		k.screenYes("Mid body", 0.95)
		k.draftReply("Mid body", map[string]any{"title": "预算验收：短邮件", "kind": "task", "summary": "三千字符"})
		k.setCase("src_acc_raw", "mid-body")
		mid := k.disposed("src_acc_raw", "mid-body-1")
		midBody := asString(k.stubOut("mid-body")[0]["detail"])
		eq(t, "mid-body 正文字符数", runes(midBody), 3000)
		short := k.item(mid.Item)
		eq(t, "短事件事项正文是完整原文", short.Body, midBody)
	})

	k.step("ACT-BUD-08 来源对象更新刷新事项正文时同样按 4000 规则", func(t *testing.T) {
		body := func(id string) string {
			var out struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal([]byte(k.web("GET", "/v1/source-events/"+id+"?part=body", "", 200)), &out); err != nil {
				t.Fatal(err)
			}
			return out.Body
		}
		newRaw := asString(k.stubOut("big-body-update")[0]["detail"])
		if runes(newRaw) != 204800 || newRaw[:100] == bigRaw[:100] {
			t.Fatal("SRC_STUB 的 big-body-update 应是 200 KiB 且开头与旧版本不同")
		}

		// 邮件来源：内置规则 mail-received 声明 on_update: attach，同关联键的新版本只记为挂靠消息，事项正文保持 4000 字符的写法，不被整份复制。
		k.setCase("src_acc_raw", "big-body-update")
		reply := k.disposed("src_acc_raw", "big-big-body-update")
		eq(t, "邮件来源的更新版本", reply.Disposition, "attached")
		eq(t, "邮件来源的更新版本挂在原事项上", reply.Item, bigItem)
		eq(t, "邮件来源的事项正文不变", k.item(bigItem).Body, proposalBody(bigRaw, bigEv.ID))

		// 来源对象的更新：规则没有声明 on_update 的来源，同关联键的新版本刷新它建出的事项，正文同样只写前 4000 个字符并链到新版本的原文页。
		// 用 SRC_STUB 的 big-body 与 big-body-update 的输出，换成自己的标题、发送者与关联键，免得与上面邮件来源的建议按标题并成一件。
		object := func(name string) map[string]any {
			e := k.stubOut(name)[0]
			e["subject"], e["sender"], e["correlation_keys"] = "Object big body", "carol@example.com", []string{"obj-big"}
			return e
		}
		k.addStub("src_acc_obj", "acc-obj", "full", "full")
		k.setJSONL("src_acc_obj", "obj-v1", object("big-body"))
		first := k.disposed("src_acc_obj", "big-big-body")
		eq(t, "来源对象的首个版本", first.Disposition, "proposed")
		if first.Item == "" || first.Item == bigItem {
			t.Fatalf("来源对象的首个版本应建出自己的事项: %+v", first)
		}
		eq(t, "首个版本的事项正文", k.item(first.Item).Body, proposalBody(bigRaw, first.ID))
		k.setJSONL("src_acc_obj", "obj-v2", object("big-body-update"))
		upd := k.disposed("src_acc_obj", "big-big-body-update")
		eq(t, "来源对象的更新版本挂在同一事项上", upd.Item, first.Item)
		want := proposalBody(newRaw, upd.ID)
		k.until("事项正文刷新为新版本的前 4000 个字符", func() bool { return k.item(first.Item).Body == want })
		if got := k.item(first.Item).Body; !strings.Contains(got, "原文较长") || runes(got) > 4300 {
			t.Errorf("刷新后的事项正文应保留\"原文较长\"与链接且不整份复制: %d 字符", runes(got))
		}
		// 账本里两个版本的原文都没变。
		eq(t, "旧版本原文的 sha256", sha(body(first.ID)), sha(bigRaw))
		eq(t, "新版本原文的 sha256", sha(body(upd.ID)), sha(newRaw))
	})
}

// proposalBody 是事项正文的期望值（§9.3）：原文前 4000 个字符，末尾注明"原文较长"并链到来源原文页。
func proposalBody(detail, event string) string {
	return string([]rune(detail)[:4000]) + fmt.Sprintf("\n\n（原文较长，这里只写前 4000 个字符，完整原文见[来源原文页](/sources/events/%s)）", event)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func asString(v any) string { s, _ := v.(string); return s }

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

type auditEv struct {
	Type    string          `json:"type"`
	Object  string          `json:"object"`
	Payload json.RawMessage `json:"payload"`
}

func (k *kit) audit(typ string) []auditEv {
	k.t.Helper()
	var out struct {
		Events []auditEv `json:"events"`
	}
	if err := json.Unmarshal([]byte(k.web("GET", "/v1/audit/events?limit=500&type="+typ, "", 200)), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Events
}

type disposedView struct {
	Material *mat `json:"material"`
}

// disposedPayload 是 GET /v1/audit/events 里该来源事件的 inbound.disposed 载荷。
func (k *kit) disposedPayload(event string) disposedView {
	k.t.Helper()
	var out struct {
		Events []struct {
			Payload disposedView `json:"payload"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(k.web("GET", "/v1/audit/events?limit=50&type=inbound.disposed&object="+event, "", 200)), &out); err != nil {
		k.t.Fatal(err)
	}
	if len(out.Events) == 0 {
		k.t.Fatalf("事件 %s 没有 inbound.disposed", event)
	}
	return out.Events[len(out.Events)-1].Payload
}
