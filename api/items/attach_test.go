package items

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) proposal(title string) string {
	return e.create(e.web, `{"kind":"task","title":"`+title+`","summary":"s"}`).ID
}

func (e *env) openItem(title string) string {
	id := e.create(e.web, `{"kind":"task","title":"`+title+`","phase":"open"}`).ID
	return id
}

func (e *env) phase(id string) domain.Phase { return e.get(id).Item.Phase }

// ACT-PRH-08、11：POST /items/{id}/attach 的成功与目标不合法的拒绝（404/409，建议不变）。
func TestAttachRoute(t *testing.T) {
	e := setup(t)
	pc, other, target := e.proposal("PC"), e.proposal("other"), e.openItem("目标")
	done := e.openItem("已完成")
	if rec := e.do("POST", "/v1/items/"+done+"/transition", `{"to":"done","reason":"finished"}`, e.web); rec.Code != 200 {
		t.Fatalf("close: %d %s", rec.Code, rec.Body)
	}
	for body, want := range map[string]int{
		`{"to":"` + done + `"}`: 409, `{"to":"itm-not-exist"}`: 404, `{"to":"` + other + `"}`: 409, `{}`: 400, `{"to":"x","id":"y"}`: 400,
	} {
		if rec := e.do("POST", "/v1/items/"+pc+"/attach", body, e.web); rec.Code != want {
			t.Errorf("%s: %d, want %d: %s", body, rec.Code, want, rec.Body)
		}
	}
	if e.phase(pc) != domain.PhaseProposed {
		t.Fatal("rejected attach changed the proposal")
	}

	// 补到已有事项是立项（二档）：只整理的 Agent 为 23，放开拍板的 Agent 直接生效。
	if rec := e.do("POST", "/v1/items/"+pc+"/attach", `{"to":"`+target+`"}`, e.token(auth.Delegate, "")); rec.Code != 403 || code(t, rec) != domain.CodeTierClosed {
		t.Fatalf("delegate with tier closed: %d %s", rec.Code, rec.Body)
	}
	if e.phase(pc) != domain.PhaseProposed {
		t.Fatal("a refused attach changed the proposal")
	}
	rec := e.do("POST", "/v1/items/"+pc+"/attach", `{"to":"`+target+`"}`, e.agent("agent_a", policy.DefaultCard().With(policy.GroupDecide)))
	if rec.Code != 200 {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body)
	}
	g := e.get(pc)
	if g.Item.Phase != domain.PhaseDropped || len(g.Relations) != 1 || g.Relations[0].Type != domain.RelRelates || g.Relations[0].To != target {
		t.Fatalf("attached proposal: %+v", g)
	}
	rec = e.do("GET", "/v1/items/"+target, "", e.web)
	if !strings.Contains(rec.Body.String(), "PC") {
		t.Fatalf("target must carry a progress entry with the proposal title: %s", rec.Body)
	}
}

type batchResp struct {
	Results []inbound.BatchResult `json:"results"`
}

// ACT-PRH-15、17：POST /proposals/batch 逐条各自成败（批量等于逐条）；形态错误、超上限整批不生效。
func TestBatchRoute(t *testing.T) {
	e := setup(t)
	a, b, c := e.proposal("a"), e.proposal("b"), e.proposal("c")
	target := e.openItem("目标")
	plan := fmt.Sprintf(`{"mode":"explicit","actions":[{"id":"%s","do":"accept"},{"id":"%s","do":"dismiss","reason":"不需要"},{"id":"%s","do":"attach","to":"%s"},{"id":"itm-not-exist","do":"accept"}]}`, a, b, c, target)

	// 批量等于逐条：没有整批的门，每条按同一规则判定；只整理的 Agent 每条都是 23，什么也不处理。
	var closed batchResp
	rec := e.do("POST", "/v1/proposals/batch", plan, e.token(auth.Delegate, ""))
	_ = json.Unmarshal(rec.Body.Bytes(), &closed)
	if rec.Code != 200 || len(closed.Results) != 4 || closed.Results[3].Code != domain.CodeNotFound {
		t.Fatalf("tier closed: %d %s", rec.Code, rec.Body)
	}
	for _, r := range closed.Results[:3] {
		if r.OK || r.Code != domain.CodeTierClosed {
			t.Fatalf("each entry is judged on its own and refused: %s", rec.Body)
		}
	}
	var many []string
	for range inbound.MaxBatch + 1 {
		many = append(many, fmt.Sprintf(`{"id":"%s","do":"accept"}`, a))
	}
	if rec := e.do("POST", "/v1/proposals/batch", `{"mode":"explicit","actions":[`+strings.Join(many, ",")+`]}`, e.web); rec.Code != 400 {
		t.Fatalf("over the limit: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{`{}`, `{"mode":"nope"}`, `not json`, `{"mode":"explicit","actions":[],"x":1}`} {
		if rec := e.do("POST", "/v1/proposals/batch", bad, e.web); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	for _, id := range []string{a, b, c} {
		if e.phase(id) != domain.PhaseProposed {
			t.Fatalf("a rejected batch processed %s", id)
		}
	}

	rec = e.do("POST", "/v1/proposals/batch", plan, e.agent("agent_a", policy.DefaultCard().With(policy.GroupDecide)))
	if rec.Code != 200 {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body)
	}
	var out batchResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Results) != 4 || !out.Results[0].OK || !out.Results[1].OK || !out.Results[2].OK || out.Results[3].OK || out.Results[3].Code != domain.CodeNotFound {
		t.Fatalf("results: %s", rec.Body)
	}
	if e.phase(a) != domain.PhaseOpen || e.phase(b) != domain.PhaseDropped || e.phase(c) != domain.PhaseDropped {
		t.Fatal("dispositions do not follow the plan")
	}
	// 逐条撤销：只撤 b 的丢弃。
	var seq int64
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, b)
		seq = es[len(es)-1].Event.Seq
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("POST", "/v1/items/"+b+"/revert", fmt.Sprintf(`{"event":%d}`, seq), e.web); rec.Code != 200 {
		t.Fatalf("revert: %d %s", rec.Code, rec.Body)
	}
	if e.phase(b) != domain.PhaseProposed || e.phase(a) != domain.PhaseOpen || e.phase(c) != domain.PhaseDropped {
		t.Fatal("revert touched other entries")
	}
}
