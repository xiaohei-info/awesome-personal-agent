package stubharness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var itemID = regexp.MustCompile(`itm_[0-9a-f]+`)

// entryKind 按附录 A.5 的五条顺序判定会话类型；没选入口（无 STUB_HARNESS_ENTRY 与覆盖项）时 ok=false。
func entryKind(e map[string]string) (string, bool) {
	platform, chat, sender, direct := e["STUB_HARNESS_PLATFORM"], e["STUB_HARNESS_CHAT_TYPE"], e["STUB_HARNESS_SENDER"], e["STUB_HARNESS_DIRECT_PLATFORMS"]
	switch e["STUB_HARNESS_ENTRY"] {
	case "":
		if platform == "" && chat == "" && sender == "" && direct == "" {
			return "", false
		}
	case "terminal":
		def(&platform, "cli")
	case "dm":
		def(&platform, "chat")
		def(&chat, "dm")
		def(&sender, "owner")
	case "group":
		def(&platform, "chat")
		def(&chat, "group")
		def(&sender, "member")
	case "listed":
		def(&platform, "ownchat")
		def(&chat, "dm")
		def(&direct, platform)
	default:
		return "unknown", true
	}
	switch chat {
	case "group", "channel", "thread":
		return "group", true
	}
	switch platform {
	case "", "cli", "tui", "local":
		return "direct", true
	}
	for _, p := range strings.Split(direct, ",") {
		if strings.TrimSpace(p) == platform {
			return "direct", true
		}
	}
	if chat == "dm" && sender != "" {
		return "direct", true
	}
	return "unknown", true
}

func def(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

// writeBrief 读运行目录的资料与原文文件、保存副本，再把 sections 写回事项（periodOnly 时只写 period）。
func writeBrief(e map[string]string, env, args []string, dir string, periodOnly bool) error {
	raw, err := os.ReadFile(filepath.Join(dir, "materials.json"))
	if err != nil {
		return err
	}
	var m struct {
		Materials []struct{ Name, Body string }
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("materials.json: %w", err)
	}
	save := e["STUB_HARNESS_SAVE_DIR"]
	if save == "" {
		save = filepath.Join(dir, "stub-harness-saved")
	}
	if err := os.MkdirAll(save, 0o755); err != nil {
		return err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		n := f.Name()
		if f.IsDir() || n == "prompt.md" || n == "output.txt" || strings.HasPrefix(n, "stub-harness-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(save, n), b, 0o644); err != nil {
			return err
		}
	}

	var sections []map[string]string
	for _, x := range m.Materials {
		var segs map[string]json.RawMessage
		if json.Unmarshal([]byte(x.Body), &segs) != nil {
			continue
		}
		names := make([]string, 0, len(segs))
		for k := range segs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			var entries []map[string]any
			if json.Unmarshal(segs[k], &entries) != nil {
				continue
			}
			var titles []string
			for _, en := range entries {
				if t, _ := en["title"].(string); t != "" {
					titles = append(titles, t)
				}
			}
			body := "无"
			if len(titles) > 0 {
				body = strings.Join(titles, "；")
			}
			sections = append(sections, map[string]string{"title": k, "body": body})
		}
	}
	spec := map[string]any{"period": "stub"}
	if !periodOnly {
		spec["sections"] = sections
	}
	item := e["STUB_HARNESS_ITEM"]
	if item == "" {
		item = itemID.FindString(strings.Join(args, " "))
	}
	apa := e["STUB_HARNESS_APA"]
	if item == "" || apa == "" {
		return fmt.Errorf("brief 需要事项 id（提示词或 STUB_HARNESS_ITEM）与 STUB_HARNESS_APA")
	}
	b, _ := json.Marshal(spec)
	cmd := exec.Command(apa, "item", "update", item, "--spec", string(b), "--reason", "stub-harness brief")
	cmd.Dir, cmd.Env = dir, env
	if o, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apa item update: %v\n%s", err, o)
	}
	return nil
}
