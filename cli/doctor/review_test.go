package doctor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ACT-UPG-02 的"只读"：经 client.Main 跑 --upgrade-check，不执行来源适配器、插件自检命令，不探测模型端点；目录哈希不变，退出码 0。
func TestUpgradeCheckThroughMainExecutesNothing(t *testing.T) {
	e, cls, gen := healthy(t)
	marker := filepath.Join(t.TempDir(), "ran")
	touch := e.script("touch-marker", "touch "+marker+"\nexit 0\n")
	e.addSource("doc-mail", "script", "mail", touch, "meta")
	e.declare(executors.Declaration{Harness: "probe", Selfcheck: &executors.Selfcheck{Command: []string{touch}}, Executors: []executors.Executor{{ID: "x", Kind: "human"}}})
	e.seal()
	before := tree(t, e.home)
	UseRegistry(func() (*store.Registry, error) { return e.reg, nil })
	var out, errb bytes.Buffer
	code := client.Main([]string{"doctor", "--upgrade-check", "--json"}, client.Env{Getenv: func(k string) string { return map[string]string{"APA_HOME": e.home}[k] },
		UserHome: t.TempDir(), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	var rep kdoctor.Report
	must(t, json.Unmarshal(out.Bytes(), &rep))
	if find(t, rep, "schema_version").Name == "" || len(rep.Checks) != 5 {
		t.Fatalf("report: %s", out.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an adapter or selfcheck command was executed by --upgrade-check")
	}
	if len(cls.Batches()) != 0 || len(gen.Requests()) != 0 {
		t.Fatal("--upgrade-check probed the model endpoints")
	}
	if tree(t, e.home) != before {
		t.Fatal("the data directory changed")
	}
}

// 升级前留下的 follow-up 用户包（v0.1 夹具的写法）：pack.yaml 里有 do[]，提示词里有已移除字段、缺 on_undecided。
// 包的问题一次列全：do[]、confidence_threshold、escalate_to、on_undecided 都出现，--upgrade-check 与能力包检查一致。
func TestRejectedPackListsEveryOffendingField(t *testing.T) {
	files := map[string]string{
		"pack.yaml":                    "name: follow-up\nversion: 1.0.0\ndescription: d\nclock_rules:\n  - id: follow-up-interval\n    type: follow_up\n    schedule: 72h\n    mode: observe\n    do:\n      - action: call_agent\n        prompt: follow-up-judge\n",
		"prompts/follow-up-judge.md":   "---\nagent: judge\nquestion_type: choice\nchoices: [remind, wait, abandon, need_explanation]\nconfidence_threshold: 0.85\nescalate_to: follow-up-explain\ntimeout: 30s\n---\n事项 {item_id}\n",
		"prompts/follow-up-explain.md": "---\nagent: generate\ntimeout: 60s\nescalate_to: follow-up-research\noutput_schema:\n  type: object\n  required: [insufficient_info]\n  properties:\n    insufficient_info: {type: boolean}\n---\n事项 {item_id}\n",
	}
	check := func(why string) {
		t.Helper()
		for _, w := range []string{"do", "confidence_threshold", "escalate_to", "on_undecided"} {
			if !strings.Contains(why, w) {
				t.Errorf("reason lacks %q: %s", w, why)
			}
		}
	}
	write := func(home string) {
		for name, body := range files {
			p := filepath.Join(home, "packs", "follow-up", name)
			must(t, os.MkdirAll(filepath.Dir(p), 0o755))
			must(t, os.WriteFile(p, []byte(body), 0o644))
		}
	}
	home, reg := v2Home(t)
	must(t, os.RemoveAll(filepath.Join(home, "packs")))
	write(home)
	d, ok := find(t, UpgradeCheck(bg, Input{Home: home, Reg: reg, Now: now}), "pack_compat").Find("follow-up")
	if !ok {
		t.Fatal("follow-up pack not reported by --upgrade-check")
	}
	check(d.Why)

	e := newEnv(t)
	write(e.home)
	e.seal()
	d, ok = find(t, Run(bg, e.input()), kdoctor.Packs).Find("follow-up")
	if !ok {
		t.Fatal("follow-up pack not reported by the packs check")
	}
	check(d.Why)
}

// 时限真正生效：命令把长命的子进程留在后台占着输出管道时，也在时限加宽限内返回，不挂住 apa doctor。
func TestSelfcheckAndProbeReturnWithinTheBoundWhenAChildLingers(t *testing.T) {
	e := newEnv(t)
	slow := e.script("lingering", "sleep 60 &\nsleep 60\n")
	start := time.Now()
	if _, err := runSelfcheck(bg, []string{slow}, os.Environ()); err == nil {
		t.Fatal("a never-finishing selfcheck must fail")
	}
	if d := time.Since(start); d > selfcheckTimeout+3*time.Second {
		t.Fatalf("selfcheck took %s", d)
	}
	start = time.Now()
	problem, _ := probeOne(bg, e.input(), domainSource(slow))
	if !strings.Contains(problem, "in time") {
		t.Fatalf("probe: %q", problem)
	}
	if d := time.Since(start); d > probeTimeout+3*time.Second {
		t.Fatalf("probe took %s", d)
	}
}

func domainSource(cmd string) domain.Source { return domain.Source{ID: "slow", Command: cmd} }
