package items_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
)

func (k *kernel) proposal(title string) string {
	k.t.Helper()
	return k.idOf(k.ok(auth.Worker, "propose", "--kind", "task", "--title", title))
}

// ACT-PRH-08、11：apa attach 把建议补到已有事项（二档）；只整理的 Agent 为 23；目标不合法时退出码 5/6 且建议不变。
func TestAttachCommand(t *testing.T) {
	k := newKernel(t)
	pa, pb := k.proposal("合同第二版意见"), k.proposal("另一条")
	target := k.openItem()
	done := k.openItem()
	k.own("item.transition", map[string]any{"id": done, "to": "done", "reason": "finished"})

	if r := k.apa(auth.Delegate, "attach", pa, "--to", target); r.code != 23 {
		t.Fatalf("tier closed: exit %d %s", r.code, r.errOut)
	}
	for to, want := range map[string]int{done: 6, "itm-not-exist": 5, pb: 6} {
		if r := k.apa(decider, "attach", pa, "--to", to); r.code != want {
			t.Errorf("--to %s: exit %d, want %d (%s)", to, r.code, want, r.errOut)
		}
	}
	if r := k.apa(auth.Delegate, "attach", pa); r.code != 2 {
		t.Fatalf("missing --to: exit %d", r.code)
	}
	if k.phase(pa) != "proposed" {
		t.Fatal("rejected attach changed the proposal")
	}
	k.ok(decider, "attach", pa, "--to", target)
	if k.phase(pa) != "dropped" {
		t.Fatalf("phase %s", k.phase(pa))
	}
	if r := k.apa(auth.Worker, "attach", pb, "--to", target); r.code != 4 {
		t.Fatalf("worker cannot answer proposals: exit %d", r.code)
	}
}

// ACT-PRH-15、17：apa proposals apply 按处置清单批量，每条各自成败：批量等于逐条，没有整批的门；
// 只整理的 Agent 每条都是 tier_closed、什么也不处理。
func TestProposalsApplyFile(t *testing.T) {
	k := newKernel(t)
	e1, e2, e3, e4 := k.proposal("e1"), k.proposal("e2"), k.proposal("e3"), k.proposal("e4")
	target := k.openItem()
	plan := filepath.Join(t.TempDir(), "plan.json")
	body := `{"actions":[{"id":"` + e1 + `","do":"accept"},{"id":"` + e2 + `","do":"dismiss","reason":"不需要"},{"id":"` + e3 + `","do":"attach","to":"` + target + `"},{"id":"itm-not-exist","do":"accept"}]}`
	if err := os.WriteFile(plan, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	r := k.ok(auth.Delegate, "proposals", "apply", "--file", plan)
	for i, m := range r.json(t)["results"].([]any) {
		want := "tier_closed"
		if i == 3 {
			want = "not_found"
		}
		if e := m.(map[string]any); e["ok"] != false || e["code"] != want {
			t.Errorf("tier closed, result %d: %v", i, e)
		}
	}
	for _, id := range []string{e1, e2, e3} {
		if k.phase(id) != "proposed" {
			t.Fatalf("a refused entry processed %s", id)
		}
	}
	for _, args := range [][]string{{"proposals"}, {"proposals", "apply"}, {"proposals", "apply", "--recommended", "--file", plan}, {"proposals", "apply", "--file", plan, "--source", "x"}, {"proposals", "apply", "--file", "/nonexistent"}} {
		if r := k.apa(auth.Delegate, args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2", args, r.code)
		}
	}

	r = k.ok(decider, "proposals", "apply", "--file", plan)
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
	r = k.ok(decider, "proposals", "apply", "--recommended")
	if res, _ := r.json(t)["results"].([]any); len(res) != 0 || k.phase(e4) != "proposed" {
		t.Fatalf("recommended with no recommendations: %s", r.out)
	}
}

// ACT-CLI-04：--file - 从标准输入读处置清单，逐条输出，与读文件同结果。
func TestProposalsApplyFileFromStdin(t *testing.T) {
	k := newKernel(t)
	e1 := k.proposal("e1")
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": k.tokens[decider], "APA_SESSION": "s1", "APA_HOME": t.TempDir()}
	plan := `{"actions":[{"id":"` + e1 + `","do":"accept"},{"id":"itm-not-exist","do":"accept"}]}`
	code := client.Main([]string{"proposals", "apply", "--file", "-"}, client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: t.TempDir(),
		Stdin: strings.NewReader(plan), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	if code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	var res struct{ Results []map[string]any }
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res.Results) != 2 || res.Results[0]["ok"] != true || res.Results[1]["ok"] != false {
		t.Fatalf("results: %s", out.String())
	}
	if k.phase(e1) != "open" {
		t.Fatal("stdin plan not applied")
	}
	code = client.Main([]string{"proposals", "apply", "--file", "-"}, client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: t.TempDir(),
		Stdin: strings.NewReader("not json"), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	if code != 2 {
		t.Fatalf("bad stdin plan: exit %d", code)
	}
}
