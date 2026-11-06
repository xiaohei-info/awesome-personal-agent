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

// accRules 是 ACT-ALW-09 的用户能力包：一条 create_item 规则，把子事项指派给 acc-work 名单之外的执行者。
// 触发事项取 acc-work 的事项，规则建出的事项项目取自触发事项。
const accRules = `name: acc-rules
version: 1.0.0
description: 验收用规则包：规则动作建子事项时也要过项目允许的执行者
event_rules:
  - id: spawn-foreign
    mode: observe
    when:
      event: item.phase_changed
      to_phase: open
      tag_any: [acc-spawn]
    do:
      - action: create_item
        kind: task
        assignee: other-host.cli
`

const spawnRule = "acc-rules/event_rules/spawn-foreign"

// rejectedWith 返回 object 上退出码为 exit 的 command.rejected 事件。
func (k *kit) rejectedWith(object string, exit float64) []Event {
	var out []Event
	for _, ev := range k.audit(url.Values{"type": {"command.rejected"}, "object": {object}}) {
		if ev.Payload["exit"] == exit {
			out = append(out, ev)
		}
	}
	return out
}

func (k *kit) childrenCount(project string) int { return len(k.itemsOf(project)) }

// TestFlowAllowedExecutors 按验收文档 flow-allowed-executors 的顺序执行 ACT-ALW-01..10：
// 项目限定的执行者是内核硬约束，CLI、Web、API 与规则动作都绕不过去。
func TestFlowAllowedExecutors(t *testing.T) {
	k := newKit(t)
	// 用户能力包随 apa serve 启动加载：写入数据目录后重启内核（ACT-ALW-09 用）。
	packDir := filepath.Join(k.Home, "packs", "acc-rules")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.yaml"), []byte(accRules), 0o644); err != nil {
		t.Fatal(err)
	}
	k.StopKernel()
	k.StartKernelAgain()

	work, open := k.Projects["acc-work"], k.Projects["acc-open"]

	k.step("ACT-ALW-01 委派给名单外执行者返回 21", func(t *testing.T) {
		parent := k.addItem("acc-work", "")
		before, pv := k.childrenCount("acc-work"), k.version(parent)
		r := k.delegate("--parent", parent, "--assignee", otherHost)
		k.expect("DEL delegate other-host.cli", r, exitNotAllow)
		var e struct {
			Allowed []string `json:"allowed"`
			Next    string   `json:"next"`
		}
		if err := json.Unmarshal([]byte(r.Stderr), &e); err != nil || len(e.Allowed) != 1 || e.Allowed[0] != workHost || e.Next == "" {
			t.Errorf("输出应含 allowed=[work-host.cli] 和 next: %s", r.Stderr)
		}
		if got := k.childrenCount("acc-work"); got != before {
			t.Errorf("不应建出子事项: %d → %d", before, got)
		}
		if k.version(parent) != pv {
			t.Error("父事项不应改变")
		}
		if len(k.rejectedWith(parent, exitNotAllow)) != 1 {
			t.Errorf("应有一条 command.rejected(exit 21): %+v", k.rejectedWith(parent, exitNotAllow))
		}
	})

	k.step("ACT-ALW-02 指派给清单外 id 返回 5", func(t *testing.T) {
		parent := k.addItem("acc-open", "")
		before := k.childrenCount("acc-open")
		k.expect("delegate ghost.cli", k.delegate("--parent", parent, "--assignee", "ghost.cli"), exitNotFound)
		if got := k.childrenCount("acc-open"); got != before {
			t.Errorf("不应建出子事项: %d → %d", before, got)
		}
	})

	k.step("ACT-ALW-03 未设置时不限制", func(t *testing.T) {
		parent := k.addItem("acc-open", "")
		child := k.idOf("delegate other-host.cli", k.delegate("--parent", parent, "--assignee", otherHost))
		if got := k.item(child)["assignee"]; got != otherHost {
			t.Errorf("子事项 assignee = %v", got)
		}
	})

	k.step("ACT-ALW-04 空名单只能指派给本人", func(t *testing.T) {
		parent := k.addItem("acc-empty", "")
		before := k.childrenCount("acc-empty")
		k.expect("空名单委派给 work-host.cli", k.delegate("--parent", parent, "--assignee", workHost), exitNotAllow)
		if got := k.childrenCount("acc-empty"); got != before {
			t.Errorf("被拒绝的委派不应建出子事项")
		}
		child := k.idOf("委派给本人", k.delegate("--parent", parent, "--assignee", ownerExecutor))
		if got := k.item(child)["assignee"]; got != ownerExecutor {
			t.Errorf("子事项 assignee = %v", got)
		}
	})

	k.step("ACT-ALW-05 改派也校验", func(t *testing.T) {
		target := k.addItem("acc-work", workHost)
		before := k.version(target)
		r := k.Apa(k.DEL, "", "item", "update", target, "--assignee", otherHost, "--reason", "reassign", "--evidence", "owner asked")
		k.expect("CLI 改派", r, exitNotAllow)
		w := k.Web("PATCH", "/v1/items/"+target, `{"assignee":"`+otherHost+`","reason":"reassign"}`)
		var e map[string]any
		if w.Code < 400 || json.Unmarshal([]byte(w.Body), &e) != nil || e["code"] != "executor_not_allowed" || e["why"] == "" {
			t.Errorf("Web 改派应被拒绝并给出原因: %d %s", w.Code, w.Body)
		}
		if k.item(target)["assignee"] != workHost || k.version(target) != before {
			t.Error("assignee 应不变")
		}
	})

	k.step("ACT-ALW-06 不能先建后移入项目", func(t *testing.T) {
		free := k.addItem("", otherHost)
		before := k.version(free)
		k.expect("移入 acc-work", k.Apa(k.DEL, "", "item", "update", free, "--project", work, "--reason", "move", "--evidence", "owner asked"), exitNotAllow)
		if p, ok := k.item(free)["project"]; ok || k.version(free) != before {
			t.Errorf("project 应不变，实际 %v", p)
		}
	})

	k.step("ACT-ALW-07 子事项不能换项目", func(t *testing.T) {
		parent := k.addItem("acc-work", "")
		r := k.delegate("--parent", parent, "--assignee", workHost, "--project", open)
		if r.Code == 0 {
			t.Fatal("委派时指定别的项目不应成功")
		}
		child := k.idOf("正常委派", k.delegate("--parent", parent, "--assignee", workHost))
		if got := k.item(child)["project"]; got != work {
			t.Fatalf("子事项应继承父事项的项目，实际 %v", got)
		}
		if r := k.Apa(k.DEL, "", "item", "update", child, "--project", open, "--reason", "move", "--evidence", "owner asked"); r.Code == 0 {
			t.Error("改子事项的 project 不应成功")
		}
		if got := k.item(child)["project"]; got != work {
			t.Errorf("子事项仍应属 acc-work，实际 %v", got)
		}
		for id := range k.itemsOf("acc-open") {
			if k.item(id)["parent"] == parent {
				t.Error("acc-open 里不应出现 acc-work 的子事项")
			}
		}
	})

	k.step("ACT-ALW-08 只在 Web 改名单", func(t *testing.T) {
		listBefore := k.allowed("acc-work")
		r := k.Apa(k.DEL, "", "project", "update", work, "--allowed-executors", otherHost, "--evidence", "owner asked")
		k.expect("CLI 改名单", r, exitWebOnly)
		w := k.APIRaw("PATCH", "/v1/projects/"+work, `{"allowed_executors":["`+otherHost+`"]}`, k.DEL)
		if w.Code != http.StatusForbidden {
			t.Errorf("PATCH /v1/projects/{id} 带 allowed_executors: HTTP %d，期望 403 %s", w.Code, w.Body)
		}
		if got := k.allowed("acc-work"); got != listBefore {
			t.Errorf("名单被非 Web 入口改写: %s → %s", listBefore, got)
		}
		changed := func() int {
			return len(k.audit(url.Values{"type": {"project.allowed_executors_changed"}, "object": {work}}))
		}
		n := changed()
		k.webOK("PUT", "/v1/settings/projects/"+work+"/allowed-executors", `{"executors":["`+workHost+`","`+otherHost+`"]}`, 200)
		if changed() != n+1 {
			t.Error("Web 修改名单后应有 project.allowed_executors_changed")
		}
		if got := k.allowed("acc-work"); !contains(got, workHost, otherHost) {
			t.Errorf("Web 修改应生效: %s", got)
		}
		k.webOK("PUT", "/v1/settings/projects/"+work+"/allowed-executors", `{"executors":["`+workHost+`"]}`, 200) // 还原
	})

	k.step("ACT-ALW-09 规则动作也校验", func(t *testing.T) {
		if !strings.Contains(k.webOK("GET", "/v1/rules", "", 200), spawnRule) {
			t.Fatalf("用户能力包的规则没有加载: %s", k.Web("GET", "/v1/rules", "").Body)
		}
		k.webOK("PATCH", "/v1/rules/"+url.PathEscape(spawnRule)+"/mode", `{"mode":"auto"}`, 200)
		before := k.childrenCount("acc-work")
		trigger := k.idOf("触发事项", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", "trigger", "--phase", "proposed",
			"--project", work, "--tags", "acc-spawn", "--evidence", "e2e setup"))
		k.expect("触发规则", k.Apa(k.DEL, "", "item", "transition", trigger, "--to", "open", "--reason", "go", "--evidence", "owner asked"), 0)
		k.waitFor("规则动作被拒绝", 15*time.Second, func() bool {
			for _, ev := range k.audit(url.Values{"type": {"command.rejected"}}) {
				if ev.Payload["exit"] == float64(exitNotAllow) && strings.Contains(ev.Reason, "not allowed") && ev.Actor["principal"] != nil &&
					strings.Contains(ev.Actor["principal"].(string), "rule:") {
					return true
				}
			}
			return false
		})
		if got := k.childrenCount("acc-work"); got != before+1 { // 只多出触发事项本身
			t.Errorf("规则不应建出事项: acc-work 事项数 %d → %d", before, got)
		}
		for id, assignee := range k.itemsOf("acc-work") {
			if assignee == otherHost {
				t.Errorf("账本里出现了名单外的 assignee: %s", id)
			}
		}
		// 对照：同一条规则在名单未设置的 acc-open 里会建出指派给 other-host.cli 的事项，证明上面的拒绝来自名单而不是规则本身无效。
		openTrigger := k.idOf("对照触发事项", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", "trigger-open", "--phase", "proposed",
			"--project", open, "--tags", "acc-spawn", "--evidence", "e2e setup"))
		k.expect("触发对照", k.Apa(k.DEL, "", "item", "transition", openTrigger, "--to", "open", "--reason", "go", "--evidence", "owner asked"), 0)
		k.waitFor("规则在未限制的项目里建出事项", 15*time.Second, func() bool {
			for _, assignee := range k.itemsOf("acc-open") {
				if assignee == otherHost {
					return true
				}
			}
			return false
		})
	})

	k.step("ACT-ALW-10 名单变更不撤销在途工作", func(t *testing.T) {
		parent := k.addItem("acc-work", "")
		child := k.idOf("在途子事项", k.delegate("--parent", parent, "--assignee", workHost))
		n := len(k.audit(url.Values{"type": {"project.allowed_executors_changed"}, "object": {work}}))
		k.webOK("PUT", "/v1/settings/projects/"+work+"/allowed-executors", `{"executors":["`+otherHost+`"]}`, 200) // 名单不再含 work-host.cli
		if got := len(k.audit(url.Values{"type": {"project.allowed_executors_changed"}, "object": {work}})); got != n+1 {
			t.Errorf("名单变更应留下一条记录/通知，新增 %d", got-n)
		}
		if got := k.item(child)["assignee"]; got != workHost {
			t.Errorf("在途子事项不应被撤销，assignee = %v", got)
		}
		k.expect("名单外的 WRK 回执", k.Apa(k.WRK, "", "report", child, "--outcome", "done", "--summary", "finished", "--link", "https://example.invalid/acc/pr/1"), 0)
		if got := k.item(child)["phase"]; got != "done" {
			t.Errorf("回执应有效，阶段 = %v", got)
		}
		k.expect("再次委派给 work-host.cli", k.delegate("--parent", parent, "--assignee", workHost), exitNotAllow)
	})
}
