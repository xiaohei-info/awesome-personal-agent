//go:build e2e

package authz

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFlowAuthzMatrix 按验收文档 flow-authz-matrix 的顺序执行 ACT-AUTH-01..07：每级凭据只能做 §14.3 允许的操作，
// 判定都在内核。任一动作未通过即停止，后续动作不得开始。
func TestFlowAuthzMatrix(t *testing.T) {
	k := newKit(t)

	k.step("ACT-AUTH-01 observer 只读加 proposed 新建", func(t *testing.T) {
		target := k.addItem("acc-open", "")
		before := k.version(target)
		k.expect("OBS item list", k.Apa(k.OBS, "", "item", "list"), 0)
		proposed := k.idOf("OBS propose", k.Apa(k.OBS, "", "propose", "--kind", "task", "--title", "obs-proposal"))
		if got := k.item(proposed)["phase"]; got != "proposed" {
			t.Errorf("propose 建出的事项阶段 = %v，应是 proposed", got)
		}
		for label, args := range map[string][]string{
			"OBS item update":    {"item", "update", target, "--title", "hijack", "--reason", "x"},
			"OBS delegate":       {"delegate", "--parent", target, "--assignee", workHost},
			"OBS project create": {"project", "create", "--name", "obs-project"},
		} {
			k.expect(label, k.Apa(k.OBS, "", args...), exitDenied)
		}
		if k.version(target) != before || k.item(target)["title"] == "hijack" {
			t.Error("observer 的写操作被拒绝后事项不应改变")
		}
		if r := k.Apa(k.OBS, "", "project", "list"); strings.Contains(r.Stdout, "obs-project") {
			t.Error("observer 不应建出项目")
		}
	})

	k.step("ACT-AUTH-02 worker 只能改分配给自己的事项", func(t *testing.T) {
		mine, theirs := k.addItem("", workHost), k.addItem("", otherHost)
		theirsBefore := k.version(theirs)
		k.expect("WRK 改自己的事项", k.Apa(k.WRK, "", "item", "update", mine, "--title", "mine-by-wrk", "--reason", "progress"), 0)
		if got := k.item(mine)["title"]; got != "mine-by-wrk" {
			t.Errorf("自己的事项应已改动，title = %v", got)
		}
		k.expect("WRK 改别人的事项", k.Apa(k.WRK, "", "item", "update", theirs, "--title", "stolen", "--reason", "x"), exitDenied)
		k.expect("WRK 建项目", k.Apa(k.WRK, "", "project", "create", "--name", "wrk-project"), exitDenied)
		if k.version(theirs) != theirsBefore || k.item(theirs)["title"] == "stolen" {
			t.Error("worker 改了别人的事项")
		}
		if r := k.Apa(k.OBS, "", "project", "list"); strings.Contains(r.Stdout, "wrk-project") {
			t.Error("worker 建出了项目")
		}
	})

	var auth03 string // ACT-AUTH-08 沿用 ACT-AUTH-03 的事项
	k.step("ACT-AUTH-03 delegate 改别人的事项须私聊会话里的本人原话加会话引用，或授权单", func(t *testing.T) {
		target := k.addItem("acc-open", otherHost)
		auth03 = target
		unchanged := func(label string) {
			t.Helper()
			if k.item(target)["title"] == "no-basis" || k.item(target)["phase"] != "needs_input" {
				t.Errorf("%s: 修改不应生效，事项应转 needs_input", label)
			}
		}
		k.expect("不带 --evidence 也不带 --grant", k.run(k.DEL, nil, "item", "update", target, "--title", "no-basis", "--reason", "x"), exitBasis)
		unchanged("无依据")
		const words = "把标题改成 with-evidence"
		k.expect("direct 会话 + 原话", k.run(k.DEL, at("auth-s3", "direct"), "item", "update", target, "--title", "with-evidence", "--reason", "owner asked", "--evidence", words), 0)
		if k.item(target)["title"] != "with-evidence" {
			t.Error("direct 会话里带原话的修改应生效")
		}
		ev := k.lastEvent(target, "item.updated")
		if ev.Evidence != words || ev.Actor["on_behalf_of"] != "owner" || ev.Actor["session"] != "auth-s3" || ev.Actor["session_kind"] != "direct" {
			t.Errorf("Event 应含 on_behalf_of、原话、会话引用与会话类型 direct: %+v", ev)
		}
		before := k.version(target)
		k.expect("同时带 --evidence 与 --grant", k.run(k.DEL, at("auth-s3", "direct"), "item", "update", target, "--title", "both", "--reason", "x", "--evidence", words, "--grant", "gnt_none"), exitInvalid)
		if k.item(target)["title"] == "both" || k.version(target) != before {
			t.Error("两种依据同时携带不应生效")
		}
	})

	k.step("ACT-AUTH-04 吊销立即生效", func(t *testing.T) {
		var out struct {
			Credential struct {
				ID string `json:"id"`
			} `json:"credential"`
			Secret string `json:"secret"`
		}
		body := k.webOK("POST", "/v1/settings/tokens", `{"kind":"worker","label":"tmp","executor":"`+workHost+`"}`, http.StatusCreated)
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.Credential.ID == "" || out.Secret == "" {
			t.Fatalf("签发临时 worker token: %v %s", err, body)
		}
		k.expect("吊销前 apa inbox", k.Apa(out.Secret, "", "inbox"), 0)
		k.webOK("DELETE", "/v1/settings/tokens/"+out.Credential.ID, "", 200)
		k.expect("吊销后 apa inbox", k.Apa(out.Secret, "", "inbox"), exitAuth) // 不重启内核、不等待
	})

	k.step("ACT-AUTH-05 设置接口只接受 Web 登录", func(t *testing.T) {
		victim := k.addItem("acc-open", "")
		policyBefore := k.webOK("GET", "/v1/settings/policy", "", 200)
		tokensBefore := k.webOK("GET", "/v1/settings/tokens", "", 200)
		allowedBefore := k.allowed("acc-work")
		auditBefore := len(k.rejections())

		attempts := []struct{ method, path, body string }{
			{"PUT", "/v1/settings/policy", `{"persona":"hijacked"}`},
			{"POST", "/v1/settings/tokens", `{"kind":"delegate","label":"evil"}`},
			{"PUT", "/v1/settings/projects/" + k.Projects["acc-work"] + "/allowed-executors", `{"executors":["` + otherHost + `"]}`},
			{"POST", "/v1/settings/purge", `{"object":"` + victim + `"}`},
		}
		for _, a := range attempts {
			r := k.APIRaw(a.method, a.path, a.body, k.DEL)
			if r.Code != http.StatusForbidden {
				t.Errorf("DEL %s %s: HTTP %d，期望 403 %s", a.method, a.path, r.Code, r.Body)
				continue
			}
			var e map[string]any
			if json.Unmarshal([]byte(r.Body), &e) != nil || e["code"] == nil || e["why"] == "" || e["next"] == "" {
				t.Errorf("DEL %s %s: 错误体缺 code/why/next: %s", a.method, a.path, r.Body)
			}
		}
		// 验收文档字面写的是 PATCH /v1/settings/policy 与 PATCH .../allowed-executors；实现的路由是 PUT（附录 B.2 只列路径）。
		// 字面方法没有路由，同样不能生效，并如实记为文档与实现不一致（ACT-REL-03 的文档漂移，不记为功能失败）。
		for _, a := range []struct{ path, body string }{
			{"/v1/settings/policy", `{"persona":"hijacked"}`},
			{"/v1/settings/projects/" + k.Projects["acc-work"] + "/allowed-executors", `{"executors":["` + otherHost + `"]}`},
		} {
			r := k.APIRaw("PATCH", a.path, a.body, k.DEL)
			if r.Code < 400 {
				t.Errorf("DEL PATCH %s 不应成功: %d", a.path, r.Code)
			}
			t.Logf("文档漂移记录: PATCH %s 返回 HTTP %d（实现的设置路由是 PUT）", a.path, r.Code)
		}

		if got := k.webOK("GET", "/v1/settings/policy", "", 200); got != policyBefore {
			t.Errorf("策略被改动:\n前 %s\n后 %s", policyBefore, got)
		}
		if got := k.webOK("GET", "/v1/settings/tokens", "", 200); got != tokensBefore {
			t.Errorf("token 清单被改动:\n前 %s\n后 %s", tokensBefore, got)
		}
		if got := k.allowed("acc-work"); got != allowedBefore {
			t.Errorf("允许的执行者被改动: %s → %s", allowedBefore, got)
		}
		if k.item(victim)["id"] != victim {
			t.Error("物理删除被执行")
		}
		if got := len(k.rejections()) - auditBefore; got < len(attempts) {
			t.Errorf("审计页应为每次被拒各留一条 command.rejected，新增 %d 条，期望至少 %d", got, len(attempts))
		}
	})

	k.step("ACT-AUTH-06 授权矩阵的契约测试通过", func(t *testing.T) {
		_, f, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(f), "..", "..", "..")
		cmd := exec.Command("go", "test", "-count=1", "-json", "./tests/contract/authz/...")
		cmd.Dir = root
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("授权矩阵契约测试失败: %v\n%s", err, out.String())
		}
		passed := map[string]bool{}
		sc := bufio.NewScanner(&out)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var ev struct{ Action, Test string }
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Test != "" && ev.Action == "pass" {
				passed[ev.Test] = true
			}
		}
		// 矩阵每一行、每一列、表中每个动作都有断言；缺任一项即矩阵有未覆盖的格子。
		for _, name := range []string{"TestMatrixEveryRowAndColumn", "TestMatrixCoversEveryTableAction", "TestWebLevelCannotBeClaimedByAgent",
			"TestMatrixRunIDExchange", "TestSettingsRejectEveryBearerAndAcceptWebSession",
			// §14.5 会话类型规则：凭原话的第二档在 direct 之外（group、unknown、缺失）都被拒；授权单、自己的工作、读取不看会话类型。
			"TestOwnerWordsOnlyInDirectSessions", "TestSessionKindDoesNotGateGrantsOwnWorkOrReads",
			"TestDelegateDoneOnOwnersItemNeedsWordsInDirectSessionOrGrant", "TestGrantAuthorisesDoneOnlyInScope"} {
			if !passed[name] {
				t.Errorf("契约测试 %s 没有通过（或没有运行）", name)
			}
		}
	})

	k.step("ACT-AUTH-07 repos 属性按级别修改", func(t *testing.T) {
		home := k.Projects["acc-home"]
		const remote = "https://example.invalid/acc/home.git"
		// 本机检出挂在已登记的远程地址上，先由 DEL 登记远程地址（建项目、改远程地址属 delegate）。
		k.expect("DEL 增加远程地址", k.Apa(k.DEL, "", "project", "repos", "add", home, "--remote", remote, "--evidence", "owner asked"), 0)
		k.expect("WRK 增加本机检出", k.Apa(k.WRK, "", "project", "repos", "add", home, "--remote", remote, "--path", "/srv/acc/home", "--host", "work-host"), 0)
		k.expect("WRK 增加新的远程地址", k.Apa(k.WRK, "", "project", "repos", "add", home, "--remote", "https://example.invalid/acc/other.git"), exitDenied)
		show := k.Apa(k.OBS, "", "project", "show", home).Stdout
		if !contains(show, remote, "/srv/acc/home") || strings.Contains(show, "other.git") {
			t.Errorf("repos 状态不符: %s", show)
		}
		var kinds []string
		for _, ev := range k.audit(url.Values{"object": {home}}) {
			kinds = append(kinds, ev.Type)
		}
		if n := strings.Count(strings.Join(kinds, ","), "project.updated"); n < 3 { // 建项目 + 远程地址 + 本机检出
			t.Errorf("变更应都记 Event，project.updated 只有 %d 条: %v", n, kinds)
		}
	})
	k.step("ACT-AUTH-08 多人会话里的原话不能让 delegate 改别人的事项", func(t *testing.T) {
		// 先在 Web 回答内核生成的 ask，让事项回到 open。
		opts, _ := k.ask(auth03)["options"].([]any)
		if len(opts) == 0 {
			t.Fatalf("ACT-AUTH-03 留下的事项应带内核生成的 ask: %v", k.ask(auth03))
		}
		k.webOK("POST", "/v1/items/"+auth03+"/decide", `{"choice":"`+opts[0].(map[string]any)["id"].(string)+`"}`, 200)
		eq(t, "回答后阶段", k.phase(auth03), "open")
		title := k.item(auth03)["title"]
		k.rejectedNotDirect("group item update", k.run(k.DEL, at("auth-g8", "group"), "item", "update", auth03, "--title", "x", "--reason", "x", "--evidence", "本人说改"))
		eq(t, "标题不变", k.item(auth03)["title"], title)
		eq(t, "阶段", k.phase(auth03), "needs_input")
	})

	k.step("ACT-AUTH-09 delegate 无依据把本人的事项标为完成被拒（SPIKE-DONE-AUTHZ）", func(t *testing.T) {
		own := k.ownItem("acc-open", "AUTH-I9")
		k.expect("无依据 transition done", k.run(k.DEL, nil, "item", "transition", own, "--to", "done", "--reason", "x"), exitBasis)
		eq(t, "AUTH-I9 阶段", k.phase(own), "needs_input")
		if len(k.auditOf(own, "command.rejected")) == 0 {
			t.Error("应记 command.rejected")
		}
		// DEL 自己委派出去的子事项：回执标记完成缺结果证据，退出码 2。
		parent := k.addItem("acc-open", workHost)
		child := k.idOf("delegate", k.delegate("--parent", parent, "--assignee", workHost))
		before := k.version(child)
		k.expect("缺结果证据的 done", k.run(k.DEL, nil, "report", child, "--outcome", "done", "--summary", "完成"), exitInvalid)
		if k.version(child) != before || k.phase(child) == "done" {
			t.Error("缺结果证据的 done 不应被接受")
		}
	})

	k.step("ACT-AUTH-10 私聊会话原话可以把本人的事项标为完成", func(t *testing.T) {
		own := k.ownItem("acc-open", "AUTH-I10")
		k.expect("direct transition done", k.run(k.DEL, at("auth-s10", "direct"), "item", "transition", own, "--to", "done", "--reason", "x", "--evidence", "这件事做完了"), 0)
		eq(t, "AUTH-I10 阶段", k.phase(own), "done")
		ev := k.lastEvent(own, "item.phase_changed")
		if ev.Actor["on_behalf_of"] != "owner" || ev.Evidence != "这件事做完了" || ev.Actor["session"] != "auth-s10" || ev.Actor["session_kind"] != "direct" {
			t.Errorf("Event 缺依据字段: %+v", ev)
		}
	})

	k.step("ACT-AUTH-11 授权单可以把范围内本人的事项标为完成，范围外不行", func(t *testing.T) {
		g := k.grant([]string{"item.transition"}, "acc-home")
		inScope, outScope := k.ownItem("acc-home", "AUTH-I11-home"), k.ownItem("acc-open", "AUTH-I11-open")
		k.expect("范围内", k.run(k.DEL, nil, "item", "transition", inScope, "--to", "done", "--reason", "x", "--grant", g), 0)
		eq(t, "范围内阶段", k.phase(inScope), "done")
		eq(t, "Event 记的授权单", k.lastEvent(inScope, "item.phase_changed").Grant, g)
		k.expect("范围外", k.run(k.DEL, nil, "item", "transition", outScope, "--to", "done", "--reason", "x", "--grant", g), exitBasis)
		eq(t, "范围外阶段", k.phase(outScope), "needs_input")
	})

	k.step("ACT-AUTH-12 多人会话原话不能把本人的事项标为完成", func(t *testing.T) {
		own := k.ownItem("acc-open", "AUTH-I12")
		k.rejectedNotDirect("group transition done", k.run(k.DEL, at("auth-g12", "group"), "item", "transition", own, "--to", "done", "--reason", "x", "--evidence", "做完了"))
		if k.phase(own) == "done" {
			t.Error("事项不应被标为完成")
		}
	})
}
