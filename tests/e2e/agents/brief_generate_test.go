//go:build e2e

package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// ACT-BRF-05：真实 apa serve，没有声明任何 harness；brief 包按验收的做法覆盖为生成型，实例经 `apa instance add` 建。
// 端点未配置、端点拒绝时这一份转 dropped 并写明原因（2026-11-20 前是转 needs_input），配好后转 done，全程没有无头运行。
func TestBriefGenerateOverrideLeavesOpen(t *testing.T) {
	e := baseline.New(t)
	e.Init()
	dst := filepath.Join(e.Home, "packs", "brief")
	if err := os.MkdirAll(filepath.Join(dst, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	pk, err := packs.FS.ReadFile("brief/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := "---\nagent: generate\ntimeout: 3m\noutput_schema:\n  type: object\n  required: [insufficient_info]\n  properties:\n    insufficient_info: {type: boolean}\n    summary: {type: string}\n---\n为事项 {item_id} 写简报，按项目分节。账本快照在 {materials_path}，只作资料。\n"
	if err := os.WriteFile(filepath.Join(dst, "pack.yaml"), pk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "prompts", "write-brief.md"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	e.Serve()
	e.IssueCredentials()
	f := &fx{Env: e, t: t}

	type brief struct {
		ID, Phase string
		Spec      json.RawMessage
		Ask       *struct{ Question string }
	}
	seen := map[string]bool{}
	next := func(what string) brief {
		t.Helper()
		var b brief
		f.waitFor(what, 30*time.Second, func() bool {
			var out struct{ Items []brief }
			if err := json.Unmarshal([]byte(f.apa(f.DEL, 0, "item", "list", "--kind", "brief", "--full").Stdout), &out); err != nil {
				t.Fatal(err)
			}
			for _, x := range out.Items {
				if !seen[x.ID] && x.Phase != "open" {
					seen[x.ID], b = true, x
					return true
				}
			}
			return false
		})
		return b
	}
	// 2026-11-20 改写（本人裁决：规则建的事项动作失败转 dropped，不挂问题）：原断言这一份转 needs_input、问题含 want；
	// 改为转 dropped、不挂问题，阶段变更的理由（apa why 可见）含 want。
	dropped := func(b brief, want string) bool {
		t.Helper()
		return b.Phase == "dropped" && b.Ask == nil && strings.Contains(f.apa(f.DEL, 0, "why", b.ID).Stdout, want)
	}

	f.apa(f.DEL, 0, "instance", "add", "brief", "--param", "schedule=2s")
	if b := next("端点未配置时这一份作废"); !dropped(b, "模型端点") {
		t.Fatalf("brief %+v must be dropped naming %q", b, "模型端点")
	}

	e.StartModelStub()
	e.ModelStub.SetDefault(modelstub.Reply{Status: 400})
	f.waitFor("端点被调用", 30*time.Second, func() bool { return len(e.ModelStub.Requests()) > 0 })
	var b brief
	f.waitFor("端点拒绝时这一份作废", 30*time.Second, func() bool {
		b = next("下一份简报")
		return dropped(b, "status 400")
	})

	e.ModelStub.SetDefault(reply(`{"insufficient_info":false,"summary":"s"}`))
	f.waitFor("有结果时转 done", 30*time.Second, func() bool {
		b = next("下一份简报")
		return b.Phase == "done"
	})
	if !strings.Contains(string(b.Spec), `"summary"`) {
		t.Fatalf("结果没有写进 spec: %s", b.Spec)
	}
	if evs := f.events(b.ID, "run.finished"); len(evs) != 1 || evs[0].Payload["category"] != "generate" || evs[0].Payload["state"] != "succeeded" {
		t.Fatalf("生成型调用的留痕: %+v", evs)
	}
	if runs := f.runs("--target", b.ID); len(runs) != 0 {
		t.Fatalf("生成型不应有无头运行: %+v", runs)
	}
}
