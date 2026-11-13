// Command accharness 是 flow-digest-alert、flow-routine 用的 STUB_HARNESS 行为扩展：
// 按模式文件（运行时读取，测试随时切换）扮演"写简报"与"例行任务"的无头运行者。
// 只用于验收环境，不得被 apa 二进制或内核代码导入。
//
//	用法: accharness <mode-file> <apa-bin> <rec-dir> <run-dir> <prompt> [harness 附加参数...]
//
// 模式: brief（写简报）、routine（例行任务）、exit1（退出码 1）、noop（不写回直接退出 0）。
// 每次运行把资料文件、提示词与收到的参数复制到 <rec-dir>/<run-id>/，供测试核对。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	itemRe  = regexp.MustCompile(`itm_[0-9a-f]+`)
	instrRe = regexp.MustCompile(`\S*instruction\.txt`)
)

func die(code int, f string, a ...any) {
	fmt.Fprintf(os.Stderr, "accharness: "+f+"\n", a...)
	os.Exit(code)
}

func main() {
	if len(os.Args) < 6 {
		die(2, "usage: accharness <mode-file> <apa-bin> <rec-dir> <run-dir> <prompt>")
	}
	modeFile, apa, rec, runDir, prompt := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]
	raw, err := os.ReadFile(modeFile)
	if err != nil {
		die(2, "%v", err)
	}
	mode := strings.TrimSpace(string(raw))
	run := filepath.Base(runDir)
	out := filepath.Join(rec, run)
	if err := os.MkdirAll(out, 0o755); err != nil {
		die(2, "%v", err)
	}
	save := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
			die(2, "%v", err)
		}
	}
	save("mode", []byte(mode))
	save("prompt.txt", []byte(prompt))
	save("args.txt", []byte(strings.Join(os.Args[5:], "\n")))
	save("env.txt", []byte(strings.Join(os.Environ(), "\n")))
	mat, err := os.ReadFile(filepath.Join(runDir, "materials.json"))
	if err != nil {
		die(2, "%v", err)
	}
	save("materials.json", mat)
	item := itemRe.FindString(prompt)

	update := func(spec any) {
		b, _ := json.Marshal(spec)
		cmd := exec.Command(apa, "item", "update", item, "--spec", string(b), "--reason", "accharness "+mode)
		cmd.Dir = runDir
		if o, err := cmd.CombinedOutput(); err != nil {
			die(3, "apa item update: %v\n%s", err, o)
		}
	}

	summary := func() { fmt.Println(`{"session_id":"acc-` + run + `","outcome":"done"}`) }

	switch mode {
	case "exit1":
		os.Exit(1)
	case "noop":
		summary()
		return
	case "brief":
		var m struct {
			Materials []struct{ Name, Body string }
		}
		_ = json.Unmarshal(mat, &m)
		body := map[string][]map[string]any{}
		for _, x := range m.Materials {
			var b map[string][]map[string]any
			if json.Unmarshal([]byte(x.Body), &b) == nil {
				for k, v := range b {
					body[x.Name+"."+k] = v
				}
			}
		}
		var lines []string
		for _, d := range body["digest.digest"] {
			lines = append(lines, fmt.Sprintf("%v %v", d["title"], d["link"]))
		}
		var planned []string
		for _, p := range body["planned.planned"] {
			planned = append(planned, fmt.Sprintf("%v（计划 %v）", p["title"], p["planned_for"]))
		}
		sections := []map[string]string{{"project": "acc-home", "body": "计划今天的事项：" + strings.Join(planned, "；")}}
		if len(lines) > 0 {
			sections = append(sections, map[string]string{"project": "知会", "body": strings.Join(lines, "\n")})
		}
		update(map[string]any{"period": "acc", "sections": sections})
		summary()
	case "routine":
		if p := instrRe.FindString(prompt); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				die(4, "读任务说明 %s: %v", p, err)
			}
			save("instruction.txt", b)
		}
		update(map[string]any{"result": "周报草稿 " + run})
		var m struct {
			Item      struct{ Project string }
			Materials []struct{ Name, Body string }
		}
		_ = json.Unmarshal(mat, &m)
		for _, x := range m.Materials {
			var ip struct {
				Output string `json:"output"`
			}
			if x.Name == "instance_params" && json.Unmarshal([]byte(x.Body), &ip) == nil && ip.Output == "item_and_material" {
				cmd := exec.Command(apa, "material", "commit", "--from", "-", "--reason", "accharness routine", "--kind", "minutes",
					"--title", "例行任务纪要 "+run, "--project", m.Item.Project)
				cmd.Dir, cmd.Stdin = runDir, strings.NewReader("周报草稿 "+run)
				if o, err := cmd.CombinedOutput(); err != nil {
					die(5, "apa material commit: %v\n%s", err, o)
				}
			}
		}
		summary()
	default:
		die(2, "unknown mode %q", mode)
	}
}
