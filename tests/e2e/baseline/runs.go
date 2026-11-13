package baseline

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 无头运行替身 STUB_HARNESS 的模式（提示词里的 STUB_MODE=<mode>）。receipt 另用 acc-stub-receipt 声明，
// 由包装脚本补上回执命令（apa report，走运行凭据；失败时 STUB_HARNESS 退出码 3 且不写摘要）（运行子进程不继承内核环境，开关只能走提示词或脚本）。
var StubModes = []string{"receipt", "no-receipt", "exit1", "summary-only", "hang"}

const (
	runPack   = "acc-env"
	receiptHN = "acc-stub-receipt"
)

// EnableStubRuns 装上验收用能力包 acc-env（每种模式一条 event_rule：带 acc-run-<mode> 标签的事项进入 open 时调用任务型 agent），
// 声明并确认 acc-stub-receipt，重启内核热加载并把规则切到 auto。须在 DeclareStubHarness 之后调用。
func (e *Env) EnableStubRuns() {
	e.T.Helper()
	rules := "name: " + runPack + "\nversion: 1.0.0\ndescription: 验收环境基线：按模式触发 STUB_HARNESS 运行\nevent_rules:\n"
	for _, m := range StubModes {
		rules += fmt.Sprintf("  - id: run-%s\n    mode: observe\n    when:\n      event: item.phase_changed\n      to_phase: open\n      tag_any: [acc-run-%s]\n    do:\n      - action: call_agent\n        prompt: acc-run-%s\n", m, m, m)
		timeout := "1m"
		if m == "hang" {
			timeout = "4s"
		}
		e.writeHomeFile(filepath.Join("packs", runPack, "prompts", "acc-run-"+m+".md"),
			fmt.Sprintf("---\nagent: task\naccess: read\ntimeout: %s\n---\nSTUB_MODE=%s 事项 {item_id}\n", timeout, m))
	}
	e.writeHomeFile(filepath.Join("packs", runPack, "pack.yaml"), rules)

	// acc-stub-receipt：包装脚本从提示词里取事项 id，回执命令把它转 done（走运行凭据）。
	receipt := filepath.Join(e.Root, "acc-env-receipt.sh")
	wrapper := filepath.Join(e.Root, "acc-env-harness.sh")
	e.writeFile(receipt, fmt.Sprintf("#!/bin/sh\nexec %q report \"$1\" --outcome done --summary env-baseline-receipt --link https://example.invalid/acc/pr/1 \n", e.Bin["apa"]))
	e.writeFile(wrapper, fmt.Sprintf("#!/bin/sh\nitem=$(printf '%%s' \"$*\" | grep -o 'itm_[0-9a-f]*' | head -1)\nexport STUB_HARNESS_RECEIPT_CMD=\"%s $item\"\nexec %q \"$@\"\n", receipt, e.Bin["stub-harness"]))
	decl, _ := json.Marshal(map[string]any{"harness": receiptHN, "headless": map[string]any{
		"command": []string{wrapper, "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := e.Apa(e.DEL, string(decl), "declare", "-", "--evidence", "环境基线声明回执替身"); r.Code != 0 {
		e.fatal("apa declare %s: %d %s%s", receiptHN, r.Code, r.Stdout, r.Stderr)
	}
	e.webOK("POST", "/v1/settings/declarations/"+receiptHN+"/1/confirm", "", 200)

	e.StopKernel()
	e.StartKernelAgain()
	for _, m := range StubModes {
		e.webOK("PATCH", "/v1/rules/"+url.PathEscape(runPack+"/event_rules/run-"+m)+"/mode", `{"mode":"auto"}`, 200)
	}
}

func (e *Env) writeHomeFile(rel, body string) {
	e.T.Helper()
	e.writeFile(filepath.Join(e.Home, rel), body)
}

func (e *Env) writeFile(path, body string) {
	e.T.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.fatal("%v", err)
	}
	mode := os.FileMode(0o644)
	if strings.HasPrefix(body, "#!") {
		mode = 0o755
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		e.fatal("%v", err)
	}
}

// StubRun 经内核触发一次 STUB_HARNESS 运行（用 acc-stub 声明；receipt 模式用 acc-stub-receipt），
// 返回触发事项 id、运行目录和运行进程收到的环境变量记录（stub-harness-env.json）。须先 EnableStubRuns。
func (e *Env) StubRun(mode string) (item, runDir string, env map[string]string) {
	e.T.Helper()
	harness := "acc-stub"
	if mode == "receipt" {
		harness = receiptHN
	}
	e.webOK("PUT", "/v1/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	seen := map[string]bool{}
	old, _ := filepath.Glob(filepath.Join(e.Home, "runs", "*", "stub-harness-env.json"))
	for _, f := range old {
		seen[f] = true
	}
	r := e.Apa(e.DEL, "", "item", "add", "--kind", "task", "--title", "run-"+mode, "--phase", "proposed",
		"--assignee", "work-host.cli", "--project", e.Projects["acc-open"], "--tags", "acc-run-"+mode, "--evidence", "环境基线触发")
	var m struct {
		ID string `json:"id"`
	}
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &m) != nil || m.ID == "" {
		e.fatal("item add: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
	if r := e.Apa(e.DEL, "", "item", "transition", m.ID, "--to", "open", "--reason", "go", "--evidence", "环境基线触发"); r.Code != 0 {
		e.fatal("item transition: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
	// 替身用 os.WriteFile 非原子写入：文件出现不等于内容写完，轮询到可解析为止。
	var file string
	var readErr, parseErr error
	var rawLen int
	if !e.WaitFor(30*time.Second, func() bool {
		fs, _ := filepath.Glob(filepath.Join(e.Home, "runs", "*", "stub-harness-env.json"))
		for _, f := range fs {
			if seen[f] {
				continue
			}
			file = f
			raw, err := os.ReadFile(f)
			if readErr = err; err != nil {
				return false
			}
			rawLen = len(raw)
			var got map[string]string
			if parseErr = json.Unmarshal(raw, &got); parseErr != nil {
				return false
			}
			env = got
			return true
		}
		return false
	}) {
		switch {
		case file == "":
			e.fatal("经内核触发 %s 运行后，运行目录里没有环境变量记录\n%s", harness, e.ServiceLog())
		case readErr != nil:
			e.fatal("读取运行环境记录 %s 失败: %v", file, readErr)
		default:
			e.fatal("运行环境记录 %s 等待 30s 仍无法解析（原始长度 %d 字节）: %v", file, rawLen, parseErr)
		}
	}
	return m.ID, filepath.Dir(file), env
}

// RegisterScriptSource 在 Web 登记一个自定义脚本来源（本人身份），脚本由内核按周期拉起。
func (e *Env) RegisterScriptSource(id, command string) {
	e.T.Helper()
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "command": command, "schedule": "1s", "timeout": 10})
	e.webOK("POST", "/v1/sources", string(body), 201)
}

// ReplayThroughKernel 把 case 设为 name，登记一个由内核拉起的脚本来源，脚本在来源程序环境里按裸命令名
// 运行一次工具（argv 为 tool 之后的参数），把 `command -v` 的解析结果与 stdout 写到返回的目录。
// 证明"替身被来源程序使用"：解析结果必须是 TOOL_STUB_BIN 下的替身，TOOL_LOG 多出这次调用。
func (e *Env) ReplayThroughKernel(id, tool, name string, argv ...string) (resolved, stdout string) {
	e.T.Helper()
	e.SetToolCase(name)
	out := filepath.Join(e.Root, "replay-"+id)
	if err := os.MkdirAll(out, 0o755); err != nil {
		e.fatal("%v", err)
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	sh := fmt.Sprintf("#!/bin/sh\nif [ ! -f %[1]s/done ]; then\n  command -v %[2]s > %[1]s/resolved\n  %[2]s %[3]s > %[1]s/stdout 2> %[1]s/stderr\n  touch %[1]s/done\nfi\necho '{\"cursor\":\"c1\"}'\n",
		out, tool, strings.Join(quoted, " "))
	script := filepath.Join(e.Root, "replay-"+id+".sh")
	e.writeFile(script, sh)
	e.RegisterScriptSource(id, script)
	if !e.WaitFor(30*time.Second, func() bool { _, err := os.Stat(filepath.Join(out, "done")); return err == nil }) {
		e.fatal("内核没有在 30 秒内拉起来源 %s\n%s", id, e.ServiceLog())
	}
	r, _ := os.ReadFile(filepath.Join(out, "resolved"))
	o, _ := os.ReadFile(filepath.Join(out, "stdout"))
	return strings.TrimSpace(string(r)), string(o)
}
