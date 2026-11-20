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

	// 2026-11-20 改写（权限模型第二版，§14.3）：改别人的事项是一档，不再要本人原话、会话引用或授权单；
	// 调用方自报的会话类型、宿主名与原话、授权单的旧参数都被忽略，不改变结果。
	k.step("ACT-AUTH-03 delegate 改别人的事项是一档，自报的会话与原话不参与判定", func(t *testing.T) {
		target := k.addItem("acc-open", otherHost)
		k.expect("只整理的 delegate，不带任何依据", k.run(k.DELO, nil, "item", "update", target, "--title", "organize-1", "--reason", "x"), 0)
		ev := k.lastEvent(target, "item.updated")
		if ev.Actor["type"] != "agent" || ev.Actor["on_behalf_of"] != nil || ev.Evidence != "" || ev.Basis["via"] != "organize" {
			t.Errorf("一档的 Event 应记 Agent 自己、依据 organize，不带代本人与原话: %+v", ev)
		}
		k.expect("同一命令带自报的私聊、原话与授权单", k.run(k.DELO, selfReported, "item", "update", target, "--title", "organize-2", "--reason", "x", "--evidence", "本人说的", "--grant", "gnt_none"), 0)
		ev = k.lastEvent(target, "item.updated")
		if k.item(target)["title"] != "organize-2" || ev.Evidence != "" || ev.Grant != "" || ev.Actor["session_kind"] != nil || ev.Actor["on_behalf_of"] != nil {
			t.Errorf("自报内容不应进入判定或审计: %+v", ev)
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

	// 2026-11-20 改写：3b（钥匙、执行者名单、物理删除）任何钥匙都是 20（硬线，先于档位）；3a 的策略段看档位，只整理的钥匙得 23。
	k.step("ACT-AUTH-05 3b 设置只接受 Web，3a 设置看档位", func(t *testing.T) {
		victim := k.addItem("acc-open", "")
		policyBefore := k.webOK("GET", "/v1/settings/policy", "", 200)
		tokensBefore := k.webOK("GET", "/v1/settings/tokens", "", 200)
		allowedBefore := k.allowed("acc-work")
		auditBefore := len(k.rejections())

		attempts := []struct {
			token, method, path, body, reason string
		}{
			{k.DELO, "PUT", "/v1/settings/policy", `{"persona":"hijacked"}`, "tier_closed"},
			{k.DELO, "POST", "/v1/settings/tokens", `{"kind":"delegate","label":"evil"}`, "hard_line"},
			{k.DEL, "POST", "/v1/settings/tokens", `{"kind":"delegate","label":"evil"}`, "hard_line"},
			{k.DEL, "PUT", "/v1/settings/projects/" + k.Projects["acc-work"] + "/allowed-executors", `{"executors":["` + otherHost + `"]}`, "hard_line"},
			{k.DEL, "POST", "/v1/settings/purge", `{"object":"` + victim + `"}`, "hard_line"},
			{k.DEL, "PUT", "/v1/settings/agents/" + k.AgentID + "/tier", `{"tier":"rules"}`, "hard_line"},
		}
		for _, a := range attempts {
			r := k.APIRaw(a.method, a.path, a.body, a.token)
			if r.Code != http.StatusForbidden {
				t.Errorf("%s %s: HTTP %d，期望 403 %s", a.method, a.path, r.Code, r.Body)
				continue
			}
			var e map[string]any
			if json.Unmarshal([]byte(r.Body), &e) != nil || e["code"] == nil || e["why"] == "" || e["next"] == "" || e["reason"] != a.reason {
				t.Errorf("%s %s: 错误体应含 code/why/next 与 reason %s: %s", a.method, a.path, a.reason, r.Body)
			}
		}
		// 验收文档字面写的是 PATCH /v1/settings/policy 与 PATCH .../allowed-executors；实现的路由是 PUT（附录 B.2 只列路径）。
		// 字面方法没有路由，同样不能生效，并如实记为文档与实现不一致（ACT-REL-03 的文档漂移，不记为功能失败）。
		for _, a := range []struct{ path, body string }{
			{"/v1/settings/policy", `{"persona":"hijacked"}`},
			{"/v1/settings/projects/" + k.Projects["acc-work"] + "/allowed-executors", `{"executors":["` + otherHost + `"]}`},
		} {
			r := k.APIRaw("PATCH", a.path, a.body, k.DELO)
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
		// 三档的对照矩阵（身份 × 每档代表命令，CLI 与 HTTP 两个入口逐格一致）及管线、设置路由的契约都要通过。
		for _, name := range []string{"TestEntryMatrixIdentityByTier", "TestWebLevelCannotBeClaimedByAgent", "TestMatrixRunIDExchange",
			"TestSettingsRejectEveryBearerAndAcceptWebSession", "TestObserverReadsAndProposesOnly", "TestWorkerCanOnlyUpdateOwnItems",
			"TestDelegateEditsAnyItemOnTierOne", "TestDelegateReceiptsAndDoneEvidence", "TestProjectWritesFollowTheirTier",
			"TestRegisteredTiersMatchAppendixB1"} {
			if !passed[name] {
				t.Errorf("契约测试 %s 没有通过（或没有运行）", name)
			}
		}
	})

	k.step("ACT-AUTH-07 repos 属性按级别修改", func(t *testing.T) {
		home := k.Projects["acc-home"]
		const remote = "https://example.invalid/acc/home.git"
		// 本机检出挂在已登记的远程地址上。远程地址是外部接入（3b）：任何钥匙都是 20，由本人在 Web 登记（2026-11-20）。
		k.expect("DEL 增加远程地址", k.Apa(k.DEL, "", "project", "repos", "add", home, "--remote", remote), exitWebOnly)
		k.expect("本人增加远程地址", k.OwnerApa("", "project", "repos", "add", home, "--remote", remote), 0)
		k.expect("WRK 增加本机检出", k.Apa(k.WRK, "", "project", "repos", "add", home, "--remote", remote, "--path", "/srv/acc/home", "--host", "work-host"), 0)
		k.expect("WRK 增加新的远程地址", k.Apa(k.WRK, "", "project", "repos", "add", home, "--remote", "https://example.invalid/acc/other.git"), exitWebOnly)
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
	// 2026-11-20 改写：把本人的事项标为完成不离开"建议"或"等你决定"，是一档；回执的 done 仍要结果证据。
	k.step("ACT-AUTH-09 delegate 把本人的事项标为完成是一档，回执 done 仍要结果证据", func(t *testing.T) {
		own := k.ownItem("acc-open", "AUTH-I9")
		k.expect("只整理的 delegate transition done", k.run(k.DELO, nil, "item", "transition", own, "--to", "done", "--reason", "x"), 0)
		eq(t, "AUTH-I9 阶段", k.phase(own), "done")
		// DEL 自己委派出去的子事项：回执标记完成缺结果证据，退出码 2。
		parent := k.addItem("acc-open", workHost)
		child := k.idOf("delegate", k.delegate("--parent", parent, "--assignee", workHost))
		before := k.version(child)
		k.expect("缺结果证据的 done", k.run(k.DEL, nil, "report", child, "--outcome", "done", "--summary", "完成"), exitInvalid)
		if k.version(child) != before || k.phase(child) == "done" {
			t.Error("缺结果证据的 done 不应被接受")
		}
	})

	k.step("ACT-AUTH-10 二档看档位：只整理得 23，放开后生效", func(t *testing.T) {
		p := k.idOf("propose", k.Apa(k.DELO, "", "propose", "--kind", "task", "--title", "auth-10-proposal"))
		k.expect("只整理接受建议", k.run(k.DELO, selfReported, "accept", p), exitBasis)
		if b := k.errBody(k.run(k.DELO, nil, "accept", p)); b["reason"] != "tier_closed" || !strings.Contains(asString(b["web_url"]), p) {
			t.Errorf("档位未放开应为 tier_closed 并指向该事项: %v", b)
		}
		eq(t, "被拒后仍是建议", k.phase(p), "proposed")
		k.expect("档位放开的 Agent 接受建议", k.run(k.DEL, nil, "accept", p), 0)
		eq(t, "接受后", k.phase(p), "open")
		if ev := k.lastEvent(p, "item.phase_changed"); ev.Basis["via"] != "allowance" || ev.Basis["agent"] != k.AgentID {
			t.Errorf("二档放行的依据应是该 Agent 的档位: %+v", ev)
		}
	})
}
