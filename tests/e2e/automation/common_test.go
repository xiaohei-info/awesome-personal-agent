//go:build e2e

// Package automation 按验收文档 flow-pack-loader（ACT-PACK-01..06）与 flow-rule-modes（ACT-RULE-01..07）
// 用真实 apa 二进制验证"能力包是声明式数据、不合规的包不生效；规则默认只观察，切 auto 只在 Web"。
// 环境来自 flow-env-baseline 的共用脚本。内核没有常驻文件监视，数据目录变化经重启 apa serve 重新加载
// （验收文档允许"触发热加载或重启"）；Web 步骤经与页面相同的 HTTP API 驱动，页面渲染由 web/ 组件测试覆盖。
package automation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

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

// reload 重启 apa serve，让数据目录里的包重新加载。
func reload(e *baseline.Env) {
	e.T.Helper()
	e.StopKernel()
	e.StartKernelAgain()
}

func step(t *testing.T, name string, f func(t *testing.T)) {
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

type ruleState struct {
	Key  string `json:"key"`
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

// rules 经 GET /v1/rules 读出全部规则状态，键为规则 id。
func rules(t *testing.T, e *baseline.Env) map[string]ruleState {
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

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}
