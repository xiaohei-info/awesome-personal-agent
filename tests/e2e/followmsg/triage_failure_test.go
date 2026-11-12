//go:build e2e

package followmsg

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const autoRule = "acc-inbound/source_rules/acc-auto"

// setModel 在 Web 设置里写模型端点；endpoint 为空表示清除（screening_timeout_ms 必须保留一个合法值）。
func (k *kit) setModel(endpoint string) {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{"model_endpoint": map[string]any{
		"url": endpoint, "model": "acc-model", "api_key": "stub-key", "screening_timeout_ms": 2000}})
	k.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
}

// unjudgedBacklog 是健康视图 unjudged_backlog 信号里因模型不可用而未判定的条数（`apa health`）。
func (k *kit) unjudgedBacklog() int {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "health")
	k.expect("apa health", r, 0)
	var h struct {
		Signals []struct {
			ID     string          `json:"id"`
			OK     bool            `json:"ok"`
			Detail json.RawMessage `json:"detail"` // 各信号的 detail 形状不同
		} `json:"signals"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &h); err != nil {
		k.t.Fatalf("health 输出不是 JSON: %q", r.Stdout)
	}
	for _, s := range h.Signals {
		if s.ID == "unjudged_backlog" {
			if s.OK {
				return 0
			}
			var d struct {
				ModelUnavailable int `json:"model_unavailable"`
			}
			if err := json.Unmarshal(s.Detail, &d); err != nil {
				k.t.Fatal(err)
			}
			return d.ModelUnavailable
		}
	}
	k.t.Fatal("健康视图没有 unjudged_backlog 信号")
	return 0
}

func (k *kit) stubRequestsAbout(subject string) int {
	n := 0
	for _, r := range k.ModelStub.Requests() {
		if strings.Contains(r.BodyText, subject) {
			n++
		}
	}
	return n
}

type auditEvent struct {
	Type   string `json:"type"`
	Object string `json:"object"`
	Reason string `json:"reason"`
	Basis  struct {
		PolicyVersion int    `json:"policy_version"`
		RuleID        string `json:"rule_id"`
	} `json:"basis"`
}

func (k *kit) audit(typ, object string) []auditEvent {
	k.t.Helper()
	var out struct {
		Events []auditEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(k.webOK("GET", "/v1/audit/events?limit=500&"+url.Values{"type": {typ}}.Encode(), "", 200)), &out); err != nil {
		k.t.Fatal(err)
	}
	var matched []auditEvent
	for _, e := range out.Events {
		if e.Object == object {
			matched = append(matched, e)
		}
	}
	return matched
}

type itemFull struct {
	Kind    string `json:"kind"`
	Phase   string `json:"phase"`
	Project string `json:"project"`
}

func (k *kit) itemFull(id string) itemFull {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "item", "show", id)
	k.expect("item show "+id, r, 0)
	var out struct {
		Item itemFull `json:"item"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Item
}

// TestFlowTriageFailure 按验收文档 flow-triage-failure 的顺序执行 ACT-TRI-01..04。
func TestFlowTriageFailure(t *testing.T) {
	k := newKit(t)
	var noModel, model500 SrcEvent

	k.step("ACT-TRI-01 判断不了时标记未判定", func(t *testing.T) {
		before := k.itemCount()
		k.setModel("") // Web 清除模型端点
		noModel = k.settled(k.emit("comment.created", "无模型端点时的评论"))
		if noModel.Disposition != "unjudged" || noModel.Item != "" {
			t.Fatalf("无模型端点应记 unjudged、不进建议流: %+v", noModel)
		}
		if n := len(k.ModelStub.Requests()); n != 0 {
			t.Errorf("端点已清除，MODEL_STUB 不应有请求，实际 %d", n)
		}

		k.ModelStub.SetDefault(modelstub.Reply{Status: 500})
		k.setModel(k.ModelStub.URL()) // 恢复端点，但模型返回 500
		model500 = k.settled(k.emit("comment.created", "模型返回500时的评论"))
		if model500.Disposition != "unjudged" || model500.Item != "" {
			t.Fatalf("模型 500 应记 unjudged、不进建议流: %+v", model500)
		}
		k.waitFor("退避重试：同一事件再次调用模型", 30*time.Second, func() bool { return k.stubRequestsAbout("模型返回500时的评论") >= 2 })

		if n := k.unjudgedBacklog(); n != 2 {
			t.Errorf("健康视图应显示两条因模型不可用的未判定积压: %d", n)
		}
		if got := k.itemCount(); got != before {
			t.Errorf("未判定事件不应进建议流: %d → %d", before, got)
		}
		for _, id := range []string{noModel.ID, model500.ID} {
			found := false
			for _, e := range k.events() {
				found = found || (e.ID == id && e.Disposition == "unjudged")
			}
			if !found {
				t.Errorf("来源页应仍有 unjudged 事件 %s", id)
			}
		}
	})

	k.step("ACT-TRI-02 手动转建议", func(t *testing.T) {
		k.ModelStub.SetDefault(modelstub.Reply{Content: `{"choice":"task","confidence":0.9}`})
		before, reqs := k.itemCount(), len(k.ModelStub.Requests())
		k.webOK("POST", "/v1/source-events/"+noModel.ID+"/propose", `{}`, 200)
		var ev SrcEvent
		for _, e := range k.events() {
			if e.ID == noModel.ID {
				ev = e
			}
		}
		if ev.Disposition != "proposed" || ev.Item == "" {
			t.Fatalf("来源事件应变 proposed 并有事项: %+v", ev)
		}
		if got := k.itemFull(ev.Item).Phase; got != "proposed" {
			t.Errorf("建议流应出现 proposed 事项，phase=%s", got)
		}
		if k.itemCount() != before+1 {
			t.Errorf("应恰好多一个事项")
		}
		if len(k.ModelStub.Requests()) != reqs {
			t.Error("手动转建议不再判断，不应调用模型")
		}
	})

	k.step("ACT-TRI-03 确定性规则不需要模型", func(t *testing.T) {
		k.setModel("")
		reqs := len(k.ModelStub.Requests())
		ev := k.settled(k.emit("issue.opened", "规则直接映射的事件"))
		if ev.Disposition != "proposed" || ev.Item == "" {
			t.Fatalf("命中 then: proposed 的规则应照常进建议流: %+v", ev)
		}
		it := k.itemFull(ev.Item)
		if it.Kind != "task" || it.Project != k.Projects["acc-work"] || it.Phase != "proposed" {
			t.Errorf("kind 和项目应由规则给出: %+v", it)
		}
		if len(k.ModelStub.Requests()) != reqs {
			t.Error("规则命中不应调用模型")
		}
	})

	k.step("ACT-TRI-04 规则自动接受走观察模式", func(t *testing.T) {
		observed := k.settled(k.emit("issue.auto", "自动接受（观察）"))
		if observed.Disposition != "proposed" || k.itemFull(observed.Item).Phase != "proposed" {
			t.Fatalf("observe 时事项应以 proposed 进入: %+v", observed)
		}
		if n := len(k.audit("rule.observed", observed.Item)); n != 1 {
			t.Errorf("应记一条\"本来会自动接受\"，实际 %d", n)
		}
		if n := len(k.audit("item.phase_changed", observed.Item)); n != 0 {
			t.Errorf("observe 时不应自动接受，实际 %d 次阶段变化", n)
		}

		k.webOK("PATCH", "/v1/rules/"+url.PathEscape(autoRule)+"/mode", `{"mode":"auto"}`, 200)
		auto := k.settled(k.emit("issue.auto", "自动接受（auto）"))
		if got := k.itemFull(auto.Item).Phase; got != "open" {
			t.Fatalf("切 auto 后应直接进 open，phase=%s", got)
		}
		changes := k.audit("item.phase_changed", auto.Item)
		if len(changes) != 1 || changes[0].Basis.RuleID != autoRule || changes[0].Basis.PolicyVersion == 0 {
			t.Errorf("Event 应记策略版本和规则 id: %+v", changes)
		}
	})
}
