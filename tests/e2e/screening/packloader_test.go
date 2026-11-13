//go:build e2e

package screening

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// wait 是等待"应当发生"的上限；内核每 5 秒热加载一次数据目录里的能力包。
const wait = 30 * time.Second

const head = "name: %s\nversion: 1.0.0\ndescription: acc\n"

func pack(name, rest string) map[string]string {
	return map[string]string{"pack.yaml": strings.Replace(head, "%s", name, 1) + rest}
}

func prompt(front string) string { return "---\n" + front + "---\n事项 {item_id}\n" }

func TestFlowPackLoader(t *testing.T) {
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	packsDir := filepath.Join(e.Home, "packs")

	packStep(t, "ACT-PACK-01 列出内置包", func(t *testing.T) {
		got := packList(t, e)
		for _, n := range []string{"brief", "review", "follow-up", "decision-card", "background-run", "guideline-draft", "agents-md", "inbound-triage", "delegation-verify", "routine"} {
			if p, ok := got[n]; !ok || p.Source != "builtin" {
				t.Errorf("缺内置包 %s: %+v", n, p)
			}
		}
	})

	packStep(t, "ACT-PACK-02 本地校验拒绝不合规的包", func(t *testing.T) {
		scratch := t.TempDir()
		bad := []struct {
			name, why string
			files     map[string]string
		}{
			{"bad-auto", "auto", pack("bad-auto", "event_rules:\n  - id: r\n    mode: auto\n    when: {event: item.created}\n    do:\n      - {action: add_tag, tag: x}\n")},
			{"bad-policy", "policy", pack("bad-policy", "policy: {web_only: false}\n")},
			{"bad-script", "executable path", pack("bad-script", "kinds:\n  - {name: bad-script-kind, title: ./run.sh}\n")},
			{"bad-action", "enumeration", pack("bad-action", "event_rules:\n  - id: r\n    mode: observe\n    when: {event: item.created}\n    do:\n      - {action: delete_everything}\n")},
			{"bad-judge-output", "output_schema", map[string]string{
				"pack.yaml":        strings.Replace(head, "%s", "bad-judge-output", 1),
				"prompts/judge.md": prompt("agent: judge\nquestion_type: yes_no\non_undecided: yes\noutput_schema: {type: object}\n"),
			}},
			{"bad-gen-tools", "tools", map[string]string{
				"pack.yaml":      strings.Replace(head, "%s", "bad-gen-tools", 1),
				"prompts/gen.md": prompt("agent: generate\noutput_schema: {type: object}\ntools: [shell]\n"),
			}},
			{"bad-placeholder", "unknown placeholder", map[string]string{
				"pack.yaml":    strings.Replace(head, "%s", "bad-placeholder", 1),
				"prompts/p.md": "---\nagent: task\n---\n读取 {secret_path}\n",
			}},
			{"bad-stats", "event_stats", map[string]string{
				"pack.yaml":    strings.Replace(head, "%s", "bad-stats", 1),
				"prompts/j.md": prompt("agent: judge\nquestion_type: yes_no\non_undecided: yes\nevent_stats: everything\n"),
			}},
			{"bad-main-answer-by", "main_session", map[string]string{
				"pack.yaml":    strings.Replace(head, "%s", "bad-main-answer-by", 1),
				"prompts/m.md": prompt("run_as: main_session\nanswer_by: [chat]\n"),
			}},
			{"bad-main-escalate", "escalate_to", map[string]string{ // v0.1 已移除字段
				"pack.yaml":    strings.Replace(head, "%s", "bad-main-escalate", 1),
				"prompts/m.md": prompt("run_as: main_session\nescalate_to: other\n"),
			}},
		}
		for _, b := range bad {
			dir := writePack(t, scratch, b.name, b.files)
			r := e.Apa(e.OBS, "", "pack", "validate", dir)
			if r.Code != 2 || !contains(r.Stderr, "pack rejected", b.why) {
				t.Errorf("坏包 %s 应被拒绝（退出码 2，原因含 %q），实际 %d: %s", b.name, b.why, r.Code, r.Stderr)
			}
		}
		good := writePack(t, scratch, "good-pack", map[string]string{
			"pack.yaml":    strings.Replace(head, "%s", "good-pack", 1) + "kinds:\n  - {name: good-kind, spec_schema: {type: object}}\n",
			"prompts/j.md": prompt("agent: judge\nquestion_type: yes_no\non_undecided: yes\nevent_stats: proposal_outcome\n"),
		})
		if r := e.Apa(e.OBS, "", "pack", "validate", good); r.Code != 0 || !strings.Contains(r.Stdout, `"ok":true`) {
			t.Errorf("合规包应通过: %d %s %s", r.Code, r.Stdout, r.Stderr)
		}
	})

	var original string
	packStep(t, "ACT-PACK-03 同名目录覆盖内置包", func(t *testing.T) {
		original = packList(t, e)["brief"].Description
		src := filepath.Join("..", "..", "..", "packs", "brief")
		dst := filepath.Join(packsDir, "brief")
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dst, "pack.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		edited := strings.Replace(string(b), "description: "+original, "description: acc-overridden", 1)
		if edited == string(b) {
			t.Fatalf("没找到可改的说明字段: %s", b)
		}
		if err := os.WriteFile(filepath.Join(dst, "pack.yaml"), []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "pack list 应显示覆盖后的说明", func() bool { return packList(t, e)["brief"].Description == "acc-overridden" })
		if p := packList(t, e)["brief"]; p.Source != "data" {
			t.Errorf("覆盖后的包应来自数据目录: %+v", p)
		}
	})

	packStep(t, "ACT-PACK-04 加载失败时保留可用的包并报健康", func(t *testing.T) {
		p := filepath.Join(packsDir, "brief", "pack.yaml")
		b, _ := os.ReadFile(p)
		bad := strings.Replace(string(b), "    type: recur\n", "    type: recur\n    mode: auto\n", 1)
		if bad == string(b) {
			t.Fatalf("没找到可改的规则: %s", b)
		}
		if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		// 热加载拒绝坏包后，健康视图出现 pack_rejections；此时再看 brief 是否仍是上一有效版本。
		waitUntil(t, "apa health 应显示能力包加载失败", func() bool {
			r := e.Apa(e.OBS, "", "health")
			var h struct {
				Signals []struct {
					ID     string          `json:"id"`
					Count  int             `json:"count"`
					Detail json.RawMessage `json:"detail"`
				} `json:"signals"`
			}
			if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &h) != nil {
				return false
			}
			for _, s := range h.Signals {
				if s.ID == "pack_rejections" {
					return s.Count == 1 && string(s.Detail) == `["brief"]`
				}
			}
			return false
		})
		got, ok := packList(t, e)["brief"]
		if !ok || got.Description != "acc-overridden" || got.Source != "data" {
			t.Errorf("应继续以上一有效版本（acc-overridden，data）工作，brief 不得消失: %+v", got)
		}
		waitUntil(t, "应产出一条健康类投递", func() bool { return packHealthDeliveries(e) == 1 })
		// 同一内容再次热加载（重写同一文件，等过至少两个 5 秒热加载周期）不得再产出第二条。
		if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(12 * time.Second)
		if n := packHealthDeliveries(e); n != 1 {
			t.Errorf("同一内容再次热加载后 brief 的健康投递应仍为 1 条，实际 %d", n)
		}
	})

	packStep(t, "ACT-PACK-05 新增自动化只写能力包", func(t *testing.T) {
		os.RemoveAll(filepath.Join(packsDir, "brief")) // 清掉上一步的坏包，只留内置
		writePack(t, packsDir, "acc-pack", map[string]string{
			"pack.yaml": strings.Replace(head, "%s", "acc-pack", 1) + `kinds:
  - name: acc-kind
    title: 验收类型
    spec_schema:
      type: object
      required: [score]
      properties:
        score: {type: integer}
event_rules:
  - id: acc-rule-1
    mode: observe
    when: {event: item.phase_changed, to_phase: open, kind: acc-kind}
    do:
      - {action: add_tag, tag: acc-seen}
`,
			"views/acc-kind.yaml": "kind: acc-kind\nlist:\n  columns: [title, spec.score]\n  filters: [tag]\ncard:\n  fields: [summary, spec.score]\n",
		})
		waitUntil(t, "pack list 应看到 acc-pack", func() bool { _, ok := packList(t, e)["acc-pack"]; return ok })
		p := packList(t, e)["acc-pack"]
		if p.Source != "data" || len(p.Kinds) != 1 || p.Kinds[0] != "acc-kind" {
			t.Fatalf("pack list 应看到 acc-pack: %+v", p)
		}
		v, ok := p.Views["acc-kind"]
		if !ok || strings.Join(v.List.Columns, ",") != "title,spec.score" || strings.Join(v.Card.Fields, ",") != "summary,spec.score" {
			t.Errorf("视图配置应原样给 Web: %+v", p.Views)
		}
		add := func(spec string) baseline.Result {
			return e.Apa(e.DEL, "", "item", "add", "--kind", "acc-kind", "--title", "acc", "--spec", spec, "--reason", "pack e2e", "--evidence", "pack e2e")
		}
		if r := add(`{"score":"high"}`); r.Code == 0 {
			t.Errorf("不符合 schema 的 spec 应被拒绝: %s", r.Stdout)
		}
		if r := add(`{"score":5}`); r.Code != 0 {
			t.Errorf("符合 schema 的 spec 应通过: %d %s", r.Code, r.Stderr)
		}
		if got := packRules(t, e)["acc-rule-1"]; got.Mode != "observe" {
			t.Errorf("包里的规则应已登记为 observe: %+v", got)
		}
	})

	packStep(t, "ACT-PACK-06 建能力包实例", func(t *testing.T) {
		r := e.Apa(e.DEL, "", "--evidence", "owner: 建简报实例", "instance", "add", "brief", "--param", "schedule=0 8 * * 1-5")
		if r.Code != 0 {
			t.Fatalf("建实例退出码 %d: %s", r.Code, r.Stderr)
		}
		if l := e.Apa(e.DEL, "", "instance", "list"); l.Code != 0 || !contains(l.Stdout, "brief", "0 8 * * 1-5") {
			t.Errorf("list 应可见实例: %d %s", l.Code, l.Stdout)
		}
		if r := e.Apa(e.DEL, "", "--evidence", "owner: 建简报实例", "instance", "add", "brief"); r.Code == 0 || !strings.Contains(r.Stderr, "schedule") {
			t.Errorf("缺 schedule 应按 instance_params 被拒绝: %d %s", r.Code, r.Stderr)
		}
		if r := e.Apa(e.OBS, "", "--evidence", "owner: 建简报实例", "instance", "add", "brief", "--param", "schedule=0 8 * * 1-5"); r.Code != 4 {
			t.Errorf("OBS 退出码应为 4，实际 %d: %s", r.Code, r.Stderr)
		}
	})
}

// writePack 把 files（相对路径 → 内容）写到 dir/<name>/ 下，整体替换同名目录。
func writePack(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(wait); ; time.Sleep(200 * time.Millisecond) {
		if ok() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s（等待 %s 仍未出现）", what, wait)
		}
	}
}

func packStep(t *testing.T, name string, f func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, f) {
		t.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}

// jsonOf 解析 CLI/HTTP 返回的 JSON。
func jsonOf(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
}

type packInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Kinds       []string
	EventRules  []struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	} `json:"event_rules"`
	Views map[string]struct {
		Kind string `json:"kind"`
		List struct {
			Columns []string `json:"columns"`
			Filters []string `json:"filters"`
		} `json:"list"`
		Card struct {
			Fields []string `json:"fields"`
		} `json:"card"`
	} `json:"views"`
}

// packList 运行 `apa pack list`（OBS 即可读）。
func packList(t *testing.T, e *baseline.Env) map[string]packInfo {
	t.Helper()
	r := e.Apa(e.OBS, "", "pack", "list")
	if r.Code != 0 {
		t.Fatalf("apa pack list 退出码 %d: %s", r.Code, r.Stderr)
	}
	var out struct {
		Packs []packInfo `json:"packs"`
	}
	jsonOf(t, r.Stdout, &out)
	m := map[string]packInfo{}
	for _, p := range out.Packs {
		m[p.Name] = p
	}
	return m
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

type ruleState struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

// packRules 经 GET /v1/rules 读出全部规则状态，键为规则 id。
func packRules(t *testing.T, e *baseline.Env) map[string]ruleState {
	t.Helper()
	r := e.APIRaw("GET", "/v1/rules", "", e.OBS)
	if r.Code != 200 {
		t.Fatalf("GET /v1/rules: %d %s", r.Code, r.Body)
	}
	var out struct {
		Rules []ruleState `json:"rules"`
	}
	jsonOf(t, r.Body, &out)
	m := map[string]ruleState{}
	for _, s := range out.Rules {
		m[s.ID] = s
	}
	return m
}

// packHealthDeliveries 数未读投递里 class=health 且 item=pack:brief 的条数。
func packHealthDeliveries(e *baseline.Env) int {
	d := e.Web("GET", "/v1/deliveries/unread", "")
	if d.Code != 200 {
		return -1
	}
	var out struct {
		Deliveries []struct {
			Class string `json:"class"`
			Item  string `json:"item"`
		} `json:"deliveries"`
	}
	if json.Unmarshal([]byte(d.Body), &out) != nil {
		return -1
	}
	n := 0
	for _, x := range out.Deliveries {
		if x.Class == "health" && x.Item == "pack:brief" {
			n++
		}
	}
	return n
}
