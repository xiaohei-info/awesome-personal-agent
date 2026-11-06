//go:build e2e

package authz

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// accRun 是 ACT-CMD-02/03 的用户能力包：带 acc-run 标签的事项进入 open 时调用任务型 agent（只有枚举动作，不含命令）。
// 任务型用哪个 harness 由 Web 设置的 default_harness 决定，所以"声明有没有被本人确认"直接决定运行能否被拉起。
const accRun = `name: acc-run
version: 1.0.0
description: 验收用规则包：事项进入 open 时调用任务型 agent
event_rules:
  - id: run-task
    mode: observe
    when:
      event: item.phase_changed
      to_phase: open
      tag_any: [acc-run]
    do:
      - action: call_agent
        prompt: acc-task
`

const accTask = `---
agent: task
access: read
timeout: 1m
---
STUB_MODE=hang 处理事项 {item_id}，账本快照在 {materials_path}。
`

const runRule = "acc-run/event_rules/run-task"

// script 在 dir 里写一个可执行的来源脚本：被拉起时留下 marker，并按来源约定在末行输出游标。
func script(t *testing.T, dir, name, marker string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "#!/bin/sh\ntouch " + marker + "\necho '{\"cursor\":\"c1\"}'\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// declaration 生成一份无头运行声明；命令只是在 marker 留下标记文件。
func declaration(harness, marker string) string {
	b, _ := json.Marshal(map[string]any{"harness": harness, "headless": map[string]any{
		"command": []string{"/bin/sh", "-c", "touch " + marker, "acc-sh", "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}},
	}})
	return string(b)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// triggerRun 建一个带 acc-run 标签的 acc-open 事项并推进到 open，触发 run-task 规则。
func (k *kit) triggerRun() string {
	k.t.Helper()
	id := k.idOf("触发事项", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", "run-trigger", "--phase", "proposed",
		"--project", k.Projects["acc-open"], "--tags", "acc-run", "--evidence", "e2e setup"))
	k.expect("触发运行", k.Apa(k.DEL, "", "item", "transition", id, "--to", "open", "--reason", "go", "--evidence", "owner asked"), 0)
	return id
}

// needsInputWith 返回阶段为 needs_input、且其事件理由含 reason 的事项数（任务型没被拉起时事项转 needs_input 并带原因）。
func (k *kit) needsInputWith(reason string) int {
	k.t.Helper()
	n := 0
	for _, ev := range k.audit(url.Values{"type": {"item.phase_changed"}}) {
		if strings.Contains(ev.Reason, reason) {
			n++
		}
	}
	return n
}

// TestFlowCommandRegistration 按验收文档 flow-command-registration 的顺序执行 ACT-CMD-01..05：
// 可执行命令只有本人能登记，agent 不能借此在主机上执行任意命令。
func TestFlowCommandRegistration(t *testing.T) {
	// 运行替身挂起 30 秒而不是永远挂起，ACT-CMD-03 在它存活期间取运行凭据，之后内核停止时不留残余进程。
	t.Setenv("STUB_HARNESS_HANG_FOR", "30s")
	k := newKit(t)
	work := t.TempDir() // 标记文件与脚本所在目录，路径不含空白
	writeFile(t, filepath.Join(k.Home, "packs", "acc-run", "pack.yaml"), accRun)
	writeFile(t, filepath.Join(k.Home, "packs", "acc-run", "prompts", "acc-task.md"), accTask)
	k.StopKernel()
	k.StartKernelAgain()

	k.step("ACT-CMD-01 delegate 不能登记自定义命令", func(t *testing.T) {
		marker := filepath.Join(work, "evil.ran")
		evil := script(t, work, "evil.sh", marker)
		r := k.Apa(k.DEL, "", "source", "add", "--id", "src_evil", "--command", evil, "--schedule", "1s", "--timeout", "10", "--evidence", "owner asked")
		k.expect("CLI 登记自定义命令", r, exitWebOnly)
		body := `{"id":"src_evil2","type":"script","command":"` + evil + `","schedule":"1s","timeout":10}`
		if w := k.APIRaw("POST", "/v1/sources", body, k.DEL); w.Code != http.StatusForbidden {
			t.Errorf("POST /v1/sources 带自定义命令: HTTP %d，期望 403 %s", w.Code, w.Body)
		}
		// 把自定义命令塞进"附带拉取程序"也不行：pull 只接受随发行附带的程序名。
		if r := k.Apa(k.DEL, "", "source", "add", "--id", "src_evil3", "--type", "pull", "--command", evil, "--schedule", "1s", "--timeout", "10", "--evidence", "owner asked"); r.Code == 0 {
			t.Error("pull 来源不应接受自定义命令")
		}
		// 登记要是生效，来源会在 1 秒节拍内被拉起；等 3 个节拍仍无标记文件才算没有执行。
		time.Sleep(3 * time.Second)
		if exists(marker) {
			t.Fatal("evil.sh 被执行了")
		}
		if list := k.Apa(k.OBS, "", "source", "list").Stdout; strings.Contains(list, "src_evil") {
			t.Errorf("来源被登记: %s", list)
		}
	})

	k.step("ACT-CMD-02 声明未确认前不生效", func(t *testing.T) {
		marker := filepath.Join(work, "pending.ran")
		decl := declaration("acc-pending", marker)
		r := k.Apa(k.DEL, decl, "declare", "-", "--evidence", "owner asked")
		k.expect("apa declare", r, 0)
		if p := k.webOK("GET", "/v1/settings/declarations", "", 200); !strings.Contains(p, "acc-pending") {
			t.Fatalf("Web 设置页应显示待确认: %s", p)
		}
		k.webOK("PUT", "/v1/settings/policy", `{"default_harness":"acc-pending"}`, 200)
		k.webOK("PATCH", "/v1/rules/"+url.PathEscape(runRule)+"/mode", `{"mode":"auto"}`, 200)

		k.triggerRun()
		const why = "没有已确认的无头运行声明"
		k.waitFor("任务型因声明未确认没有被拉起", 15*time.Second, func() bool { return k.needsInputWith(why) == 1 })
		time.Sleep(time.Second) // 给错误地拉起留出时间
		if exists(marker) {
			t.Fatal("声明未确认就执行了")
		}
		if cmds := k.webOK("GET", "/v1/settings/commands", "", 200); strings.Contains(cmds, "acc-pending") {
			t.Errorf("未确认的声明不应在生效清单里: %s", cmds)
		}

		// 对照：本人在 Web 确认后，同一触发会拉起声明的命令，证明上面没有拉起的原因就是未确认。
		k.webOK("POST", "/v1/settings/declarations/acc-pending/1/confirm", "", 200)
		k.triggerRun()
		k.waitFor("确认后命令被拉起", 20*time.Second, func() bool { return exists(marker) })
	})

	k.step("ACT-CMD-03 运行凭据不能提交声明", func(t *testing.T) {
		k.webOK("PUT", "/v1/settings/policy", `{"default_harness":"acc-stub"}`, 200) // 无头运行替身，已确认
		k.triggerRun()
		var token string
		k.waitFor("后台运行写出它收到的环境变量", 20*time.Second, func() bool {
			files, _ := filepath.Glob(filepath.Join(k.Home, "runs", "*", "stub-harness-env.json"))
			for _, f := range files {
				var env map[string]string
				if raw, err := os.ReadFile(f); err == nil && json.Unmarshal(raw, &env) == nil && env["APA_TOKEN"] != "" && env["APA_RUN"] != "" {
					token = env["APA_TOKEN"]
					return true
				}
			}
			return false
		})
		// 对照：这把运行凭据本身有效（能读账本），只是没有提交声明的权限。
		k.expect("运行凭据读账本", k.Apa(token, "", "inbox"), 0)
		before := k.webOK("GET", "/v1/settings/declarations", "", 200)
		r := k.Apa(token, declaration("acc-from-run", filepath.Join(work, "from-run.ran")), "declare", "-")
		k.expect("运行凭据 apa declare", r, exitDenied)
		if after := k.webOK("GET", "/v1/settings/declarations", "", 200); after != before || strings.Contains(after, "acc-from-run") {
			t.Errorf("声明不应被接收: %s", after)
		}
	})

	k.step("ACT-CMD-04 本人可在 Web 或主机配置文件登记", func(t *testing.T) {
		webMarker, hostMarker := filepath.Join(work, "web.ran"), filepath.Join(work, "host.ran")
		webScript, hostScript := script(t, work, "web.sh", webMarker), script(t, work, "host.sh", hostMarker)
		// Web：本人登记自定义脚本 Source 命令。
		body := `{"id":"src_webscript","type":"script","command":"` + webScript + `","schedule":"1s","timeout":10}`
		k.webOK("POST", "/v1/sources", body, 201)
		k.waitFor("Web 登记的命令被调度执行", 15*time.Second, func() bool { return exists(webMarker) })

		// 主机配置文件：sources.json 与 declarations/*.json，重启服务后按本人身份登记。
		writeFile(t, filepath.Join(k.Home, "sources.json"),
			`{"sources":[{"id":"src_hostscript","type":"script","command":"`+hostScript+`","schedule":"1s","timeout":10}]}`)
		writeFile(t, filepath.Join(k.Home, "declarations", "acc-host.json"), declaration("acc-host", filepath.Join(work, "host-decl.ran")))
		k.StopKernel()
		k.StartKernelAgain()
		k.waitFor("主机配置文件登记的命令被调度执行", 15*time.Second, func() bool { return exists(hostMarker) })
		if cmds := k.webOK("GET", "/v1/settings/commands", "", 200); !strings.Contains(cmds, "acc-host") {
			t.Errorf("主机配置文件里的声明应立即生效: %s", cmds)
		}

		// 变更可审计：两种登记都按本人身份记 Event。
		registered := map[string]bool{}
		for _, ev := range k.audit(url.Values{"type": {"source.registered"}}) {
			if ev.Actor["principal"] == "owner" {
				registered[ev.Object] = true
			}
		}
		for _, id := range []string{"src_webscript", "src_hostscript"} {
			if !registered[id] {
				t.Errorf("来源 %s 的登记应作为本人的变更记入审计: %v", id, registered)
			}
		}
	})

	k.step("ACT-CMD-05 delegate 可以登记随发行附带的拉取程序", func(t *testing.T) {
		r := k.Apa(k.DEL, "", "source", "add", "--id", "src_rss", "--type", "pull", "--command", "rss-source", "--schedule", "1h", "--timeout", "30", "--evidence", "owner asked")
		k.expect("登记 RSS 拉取程序", r, 0)
		if show := k.Apa(k.OBS, "", "source", "show", "src_rss"); show.Code != 0 || !contains(show.Stdout, `"rss-source"`, `"pull"`) {
			t.Errorf("来源应已登记: %d %s", show.Code, show.Stdout)
		}
	})
}
