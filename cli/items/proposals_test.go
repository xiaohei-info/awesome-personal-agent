package items_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
)

func (k *kernel) proposal(title string) string {
	k.t.Helper()
	return k.idOf(k.ok(auth.Worker, "propose", "--kind", "task", "--title", title))
}

// ACT-PRH-08、11：apa attach 把建议补到已有事项；目标不合法时退出码 5/6 且建议不变；缺依据退出码 23。
func TestAttachCommand(t *testing.T) {
	k := newKernel(t)
	pa, pb := k.proposal("合同第二版意见"), k.proposal("另一条")
	target := k.openItem()
	done := k.openItem()
	k.own("item.transition", map[string]any{"id": done, "to": "done", "reason": "finished"})

	if r := k.apa(auth.Delegate, "attach", pa, "--to", target); r.code != 23 {
		t.Fatalf("no evidence: exit %d %s", r.code, r.errOut)
	}
	for to, want := range map[string]int{done: 6, "itm-not-exist": 5, pb: 6} {
		if r := k.apa(auth.Delegate, "attach", pa, "--to", to, "--evidence", "补到合同谈判"); r.code != want {
			t.Errorf("--to %s: exit %d, want %d (%s)", to, r.code, want, r.errOut)
		}
	}
	if r := k.apa(auth.Delegate, "attach", pa); r.code != 2 {
		t.Fatalf("missing --to: exit %d", r.code)
	}
	if k.phase(pa) != "proposed" {
		t.Fatal("rejected attach changed the proposal")
	}
	k.ok(auth.Delegate, "attach", pa, "--to", target, "--evidence", "这条补到合同谈判")
	if k.phase(pa) != "dropped" {
		t.Fatalf("phase %s", k.phase(pa))
	}
	if r := k.apa(auth.Worker, "attach", pb, "--to", target); r.code != 4 {
		t.Fatalf("worker cannot answer proposals: exit %d", r.code)
	}
}

// ACT-PRH-15、17：apa proposals apply 按处置清单批量，每条各自成败；缺依据整批不生效。
func TestProposalsApplyFile(t *testing.T) {
	k := newKernel(t)
	e1, e2, e3, e4 := k.proposal("e1"), k.proposal("e2"), k.proposal("e3"), k.proposal("e4")
	target := k.openItem()
	plan := filepath.Join(t.TempDir(), "plan.json")
	body := `{"actions":[{"id":"` + e1 + `","do":"accept"},{"id":"` + e2 + `","do":"dismiss","reason":"不需要"},{"id":"` + e3 + `","do":"attach","to":"` + target + `"},{"id":"itm-not-exist","do":"accept"}]}`
	if err := os.WriteFile(plan, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if r := k.apa(auth.Delegate, "proposals", "apply", "--file", plan); r.code != 23 {
		t.Fatalf("no evidence: exit %d %s", r.code, r.errOut)
	}
	if r := k.apa(auth.Delegate, "proposals", "apply", "--recommended", "--source", "PRH_A"); r.code != 23 {
		t.Fatalf("recommended without evidence: exit %d %s", r.code, r.errOut)
	}
	for _, id := range []string{e1, e2, e3} {
		if k.phase(id) != "proposed" {
			t.Fatalf("a rejected batch processed %s", id)
		}
	}
	for _, args := range [][]string{{"proposals"}, {"proposals", "apply"}, {"proposals", "apply", "--recommended", "--file", plan}, {"proposals", "apply", "--file", plan, "--source", "x"}, {"proposals", "apply", "--file", "/nonexistent"}} {
		if r := k.apa(auth.Delegate, args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2", args, r.code)
		}
	}

	r := k.ok(auth.Delegate, "proposals", "apply", "--file", plan, "--evidence", "这几条按我说的处理")
	res, _ := r.json(t)["results"].([]any)
	if len(res) != 4 {
		t.Fatalf("results: %s", r.out)
	}
	for i, want := range []bool{true, true, true, false} {
		m := res[i].(map[string]any)
		if m["ok"] != want || (!want && (m["code"] != "not_found" || m["why"] == "")) {
			t.Errorf("result %d: %v", i, m)
		}
	}
	if k.phase(e1) != "open" || k.phase(e2) != "dropped" || k.phase(e3) != "dropped" || k.phase(e4) != "proposed" {
		t.Fatal("dispositions do not follow the plan")
	}
	// 没有推荐的建议，按推荐批量什么都不处理。
	r = k.ok(auth.Delegate, "proposals", "apply", "--recommended", "--evidence", "按推荐")
	if res, _ := r.json(t)["results"].([]any); len(res) != 0 || k.phase(e4) != "proposed" {
		t.Fatalf("recommended with no recommendations: %s", r.out)
	}
}
