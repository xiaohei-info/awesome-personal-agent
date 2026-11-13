//go:build e2e

package inbound

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const triRules = `name: acc-tri-rules
version: 1.0.0
description: 验收用来源规则：确定性规则与自动接受
source_rules:
  - id: tri-cal
    match: {source_type: acc, event_type: [tri.cal]}
    kind: task
    project_by: "fixed:%s"
    then: proposed
  - id: tri-auto
    match: {source_type: acc, event_type: [tri.auto]}
    kind: task
    project_by: "fixed:%s"
    then: proposed
    accept: auto
`

const triAutoRule = "acc-tri-rules/source_rules/tri-auto"

// endpoints 在 Web 设置页写两个端点：空串表示清除该端点，否则指向对应替身（local）。
func (k *kit) setEndpoints(judge, generate bool) {
	k.t.Helper()
	body := map[string]any{"screening_timeout_ms": 2000}
	if judge {
		body["judge_endpoint"] = map[string]any{"url": k.Cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1", "locality": "local"}
	} else {
		body["judge_endpoint"] = nil // JSON null 清除端点
	}
	if generate {
		body["generate_endpoint"] = map[string]any{"url": k.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": "local"}
	} else {
		body["generate_endpoint"] = nil
	}
	b, _ := json.Marshal(body)
	k.webOK("PUT", "/v1/settings/model-endpoints", string(b), 200)
}

// unjudgedBacklog 是健康视图 unjudged_backlog 信号里因模型不可用而未判定的条数。
func (k *kit) unjudgedBacklog() int {
	k.t.Helper()
	var h struct {
		Signals []struct {
			ID     string          `json:"id"`
			OK     bool            `json:"ok"`
			Detail json.RawMessage `json:"detail"`
		} `json:"signals"`
	}
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "health").Stdout), &h))
	for _, s := range h.Signals {
		if s.ID == "unjudged_backlog" {
			if s.OK {
				return 0
			}
			var d struct {
				ModelUnavailable int `json:"model_unavailable"`
			}
			k.must(json.Unmarshal(s.Detail, &d))
			return d.ModelUnavailable
		}
	}
	k.t.Fatal("健康视图没有 unjudged_backlog 信号")
	return 0
}

// rawEvent 是来源页上某个来源事件的原始 JSON 字段。
func (k *kit) rawEvent(src, eventID string) map[string]any {
	k.t.Helper()
	var out struct {
		Events []map[string]any `json:"events"`
	}
	k.must(json.Unmarshal([]byte(k.APIRaw("GET", "/v1/sources/"+src+"/events?limit=500", "", k.OBS).Body), &out))
	for _, e := range out.Events {
		if d, _ := e["dedupe_key"].(map[string]any); d["event_id"] == eventID {
			return e
		}
	}
	k.t.Fatalf("没有来源事件 %s", eventID)
	return nil
}

func (k *kit) actorOf(e auditEvent) string { return string(e.Actor) }

func TestFlowTriageFailure(t *testing.T) {
	k := newKit(t)
	k.registerInp() // ACT-INP-01 登记的 INP_SRC

	k.step("ACT-TRI-01 两个端点都未配置时规则判不了的事件记未判定", func(t *testing.T) {
		before := len(k.proposed())
		k.setEndpoints(false, false)
		k.clear()
		k.emit("tri-none-1", "tri-none 自由文本")
		ev := k.waitEvent(inpSrc, "tri-none-1", wait)
		eq(t, "disposition", ev.Disposition, "unjudged")
		eq(t, "unjudged_reason", ev.Unjudged, "model_unavailable")
		eq(t, "item", ev.Item, "")
		eq(t, "建议流条数", len(k.proposed()), before)
		if n := k.unjudgedBacklog(); n != 1 {
			t.Errorf("健康视图应有 1 条未判定积压信号，实际 %d", n)
		}
		quiet()
		eq(t, "CLS_LOG 新请求", len(k.Cls.Batches())-k.clsOff, 0)
		eq(t, "MODEL_LOG 新请求", len(k.ModelStub.Requests())-k.modelOff, 0)
	})

	k.step("ACT-TRI-02 本人手动把未判定事件转为建议，不再判断", func(t *testing.T) {
		src := k.byEventID(inpSrc, "tri-none-1")[0]
		k.clear()
		k.webOK("POST", "/v1/source-events/"+src.ID+"/propose", `{}`, 200)
		ev := k.byEventID(inpSrc, "tri-none-1")[0]
		eq(t, "disposition", ev.Disposition, "proposed")
		if ev.Item == "" {
			t.Fatal("来源事件应指向新事项")
		}
		it := k.item(ev.Item)
		eq(t, "phase", it.Phase, "proposed")
		eq(t, "kind", it.Kind, "task")
		eq(t, "project", it.Project, "")
		for _, typ := range []string{"item.created", "inbound.disposed"} {
			object := ev.Item
			if typ == "inbound.disposed" {
				object = ev.ID
			}
			es := k.auditOf(typ, object)
			if len(es) == 0 {
				t.Errorf("事件流里应有 %s（对象 %s）", typ, object)
			}
			// inbound.disposed 先有内核记的"未判定"，再有本人的"转为建议"：至少有一条出自本人。
			owner := false
			for _, e := range es {
				owner = owner || !strings.Contains(k.actorOf(e), "kernel:")
			}
			if !owner {
				t.Errorf("%s 的操作者应是本人: %v", typ, es)
			}
		}
		eq(t, "CLS_LOG 新请求", len(k.Cls.Batches())-k.clsOff, 0)
		eq(t, "MODEL_LOG 新请求", len(k.ModelStub.Requests())-k.modelOff, 0)
	})

	k.step("ACT-TRI-03 确定性规则不需要模型", func(t *testing.T) {
		k.writePack("acc-tri-rules", fmt.Sprintf(triRules, k.Projects["acc-work"], k.Projects["acc-open"]), triAutoRule)
		k.clear()
		k.emit("tri-rule-1", "tri-rule 规则直接映射的事件", func(e *Ev) { e.Type = "tri.cal" })
		ev := k.waitEvent(inpSrc, "tri-rule-1", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "phase", it.Phase, "proposed")
		eq(t, "kind", it.Kind, "task")
		eq(t, "project", it.Project, k.Projects["acc-work"])
		eq(t, "CLS_LOG 新请求", len(k.Cls.Batches())-k.clsOff, 0)
		eq(t, "MODEL_LOG 新请求", len(k.ModelStub.Requests())-k.modelOff, 0)
	})

	k.step("ACT-TRI-04 规则自动接受走观察模式", func(t *testing.T) {
		observed := k.emitSettled("tri-auto-1", "tri-auto 自动接受（观察）", "tri.auto")
		eq(t, "切换前 disposition", observed.Disposition, "proposed")
		eq(t, "切换前 phase", k.item(observed.Item).Phase, "proposed")
		eq(t, "rule.observed 条数", len(k.auditOf("rule.observed", observed.Item)), 1)
		eq(t, "切换前阶段变化次数", len(k.auditOf("item.phase_changed", observed.Item)), 0)

		k.webOK("PATCH", "/v1/rules/"+url.PathEscape(triAutoRule)+"/mode", `{"mode":"auto"}`, 200)
		auto := k.emitSettled("tri-auto-2", "tri-auto 自动接受（auto）", "tri.auto")
		eq(t, "切 auto 后 phase", k.item(auto.Item).Phase, "open")
		changes := k.auditOf("item.phase_changed", auto.Item)
		if len(changes) != 1 {
			t.Fatalf("切 auto 后应恰有一次阶段变化: %+v", changes)
		}
		var full struct {
			Events []struct {
				Object string
				Basis  struct {
					PolicyVersion int    `json:"policy_version"`
					RuleID        string `json:"rule_id"`
				}
			}
		}
		k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/audit/events?limit=500&type=item.phase_changed", "", 200)), &full))
		hit := false
		for _, e := range full.Events {
			if e.Object == auto.Item && e.Basis.RuleID == triAutoRule && e.Basis.PolicyVersion > 0 {
				hit = true
			}
		}
		if !hit {
			t.Errorf("Event 应记策略版本和规则 id: %+v", full.Events)
		}
	})

	k.step("ACT-TRI-05 两个端点都配置但都不可用时同样记未判定，不直接放进建议流", func(t *testing.T) {
		k.setEndpoints(true, true)
		before := len(k.proposed())
		k.Cls.SetFault(modelstub.ClsFault{Status: 503})
		k.ModelStub.Judge(modelstub.ChatJudge{Key: keyOf("tri-down"), Status: 500})
		k.ModelStub.When("tri-down", failReply(500))
		k.clear()
		k.emit("tri-down-1", "tri-down 自由文本")
		ev := k.waitEvent(inpSrc, "tri-down-1", wait)
		eq(t, "disposition", ev.Disposition, "unjudged")
		eq(t, "unjudged_reason", ev.Unjudged, "model_unavailable")
		eq(t, "建议流条数", len(k.proposed()), before)
		if len(k.clsAbout("tri-down")) == 0 {
			t.Error("CLS_LOG 应有这道题")
		}
		if !k.WaitFor(wait, func() bool { return len(k.modelAbout("tri-down", true)) > 0 }) {
			t.Error("MODEL_LOG 应有同题重答的判断请求")
		}
		if n := k.unjudgedBacklog(); n < 1 {
			t.Errorf("健康视图应报告模型不可用: %d", n)
		}
	})

	k.step("ACT-TRI-06 端点恢复后未判定事件自动重判并进入完整流水线", func(t *testing.T) {
		k.Cls.SetFault(modelstub.ClsFault{})
		k.screenCls("tri-down", 0.90)
		k.ModelStub.Reset()
		k.draft("tri-down 自由文本", map[string]any{"title": "tri-down 起草标题", "kind": "task"})
		before := k.rawEvent(inpSrc, "tri-down-1")["rejudges"]
		// 模型仍不可用时重判退避 10 分钟：用测试时钟越过退避，再等后台重判。
		k.TCAdvance(11 * 60)
		ev := k.waitEvent(inpSrc, "tri-down-1", wait)
		if !k.WaitFor(wait, func() bool { return k.byEventID(inpSrc, "tri-down-1")[0].Disposition == "proposed" }) {
			t.Fatalf("tri-down-1 一直没有重判: %+v\n%s", ev, k.ServiceLog())
		}
		ev = k.byEventID(inpSrc, "tri-down-1")[0]
		eq(t, "title", k.item(ev.Item).Title, "tri-down 起草标题")
		requeued := false
		for _, e := range k.auditOf("inbound.requeued", ev.ID) {
			requeued = requeued || strings.Contains(k.actorOf(e), "system")
		}
		if !requeued {
			t.Errorf("审计页应有操作者为内核的 inbound.requeued: %+v", k.auditOf("inbound.requeued", ev.ID))
		}
		n := func(v any) float64 { f, _ := v.(float64); return f }
		if after := k.rawEvent(inpSrc, "tri-down-1")["rejudges"]; n(after) != n(before)+1 {
			t.Errorf("rejudges 应加 1: %v → %v", before, after)
		}
	})

	k.step("ACT-TRI-07 只有判断端点时，进建议流的内容用来源标题、摘要留空", func(t *testing.T) {
		k.setEndpoints(true, false)
		k.screenCls("tri-clsonly", 0.90)
		k.clear()
		k.emit("tri-clsonly-1", "tri-clsonly 原始标题", func(e *Ev) { e.Summary = "原始摘要" })
		ev := k.waitEvent(inpSrc, "tri-clsonly-1", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "title", it.Title, "tri-clsonly 原始标题")
		eq(t, "summary", it.Summary, "")
		eq(t, "kind", it.Kind, "task")
		eq(t, "project", it.Project, "")
		eq(t, "MODEL_LOG 请求", k.modelAny("tri-clsonly"), 0)
		k.setEndpoints(true, true)
		k.removePack("acc-tri-rules")
	})
}

// emitSettled 追加一个指定类型的事件并等它被处置。
func (k *kit) emitSettled(id, subject, typ string) SrcEvent {
	k.emit(id, subject, func(e *Ev) { e.Type = typ })
	return k.waitEvent(inpSrc, id, wait)
}
