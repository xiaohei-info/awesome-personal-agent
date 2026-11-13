//go:build e2e

package backlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

type restorable struct {
	Matched int
	Sources []struct {
		Source string
		Count  int
	}
}

func (k *kit) preview(query string) restorable {
	k.t.Helper()
	var p restorable
	k.get("/v1/source-events/restorable?"+query, &p)
	return p
}

// setConfig 改夹具副本 config.yaml 里的一个顶层键（服务已停止时调用）。
func (k *kit) setConfig(key, value string) {
	k.t.Helper()
	path := filepath.Join(k.Home, "config.yaml")
	raw, err := os.ReadFile(path)
	k.must(err)
	var lines []string
	for _, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if !strings.HasPrefix(ln, key+":") {
			lines = append(lines, ln)
		}
	}
	lines = append(lines, key+": "+value)
	k.must(os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

// runCredential 经内核真实拉起一次无头运行替身，返回运行进程拿到的运行凭据（APA_TOKEN）。
func (k *kit) runCredential() string {
	k.t.Helper()
	k.webOK("POST", "/v1/settings/executors", `{"id":"work-host.cli","kind":"external-cli"}`, 200)
	var out struct{ ID string }
	k.must(json.Unmarshal([]byte(k.webOK("POST", "/v1/projects", `{"name":"acc-open"}`, 201)), &out))
	k.Projects["acc-open"] = out.ID
	k.DeclareStubHarness()
	k.EnableStubRuns()
	_, _, env := k.StubRun("hang")
	if env["APA_TOKEN"] == "" {
		k.t.Fatalf("运行环境里没有 APA_TOKEN: %v", env)
	}
	return env["APA_TOKEN"]
}

// TestBacklogRestoreFlow 是 flow-backlog-restore：本人把 v0.1 旧级联误丢的事件先预览再放回积压，只经四步交新的级联重判，重复执行不再放回。
func TestBacklogRestoreFlow(t *testing.T) {
	k := newKit(t, nil, true)
	k.Cls.SetDefaultPYes(0.05) // 新级联：低于不确定带下限，分类端点直接按否并留下判断记录
	k.Cls.PYes(modelstub.Key{Contains: "ignored 1"}, 0.95)
	k.Cls.PYes(modelstub.Key{Contains: "ignored 2"}, 0.95) // 一部分放回的事件分数高，会进入建议流并受每日上限约束
	yes := true
	k.ModelStub.Judge(modelstub.ChatJudge{Answer: "yes", Sufficient: &yes})
	k.ModelStub.SetDefault(modelstub.Reply{Content: draftReply})

	var (
		win         struct{ From, To string }
		total       int
		bySource    map[string]int
		first       string
		oldCascade  = map[string]bool{} // 窗口内旧判断忽略的事件 id
		controls    []string
		seq0        int64
		restoredByA int
	)

	k.step("ACT-RST-01 准备已升级的夹具副本", func(t *testing.T) {
		var oc struct {
			Window   struct{ From, To string }
			Count    int
			BySource map[string]int `json:"by_source"`
		}
		k.must(json.Unmarshal(k.fx.Raw["old_cascade_ignored"], &oc))
		win.From, win.To, total, bySource = oc.Window.From, oc.Window.To, oc.Count, oc.BySource
		if total == 0 || len(bySource) < 2 {
			t.Fatalf("窗口内旧判断忽略事件应大于 0 且分布在至少两个来源: %+v", oc)
		}
		var names []string
		for s := range bySource {
			names = append(names, s)
		}
		sort.Strings(names)
		first = names[0]
		for _, key := range []string{"rule_ignored", "user_ignored", "outside_window"} {
			var ids []string
			k.must(json.Unmarshal(k.fx.Raw[key], &ids))
			if len(ids) == 0 {
				t.Fatalf("清单缺对照组 %s", key)
			}
			controls = append(controls, ids...)
		}
		ignored := k.all("ignored")
		ctrl := map[string]bool{}
		for _, id := range controls {
			ctrl[id] = true
		}
		for _, e := range ignored {
			if !ctrl[e.ID] {
				oldCascade[e.ID] = true
			}
		}
		if len(oldCascade) != total {
			t.Fatalf("账本里窗口内旧判断忽略的事件 %d 条，清单 count=%d", len(oldCascade), total)
		}
		seq0 = k.maxSeq()
		k.clear()
	})

	q := func(extra string) string { return fmt.Sprintf("from=%s&to=%s%s", win.From, win.To, extra) }
	stillIgnored := func() map[string]bool {
		m := map[string]bool{}
		for _, e := range k.all("ignored") {
			m[e.ID] = true
		}
		return m
	}

	k.step("ACT-RST-02 升级本身没有放回任何事件", func(t *testing.T) {
		ign := stillIgnored()
		for id := range oldCascade {
			if !ign[id] {
				t.Fatalf("升级后事件 %s 不再是 ignored", id)
			}
		}
		if _, d, _ := k.unjudgedHealth(); num(d, "restored") != 0 {
			t.Fatalf("detail.restored=%d，应为 0", num(d, "restored"))
		}
	})

	k.step("ACT-RST-03 预览只读且返回条数与按来源分布", func(t *testing.T) {
		var cli restorable
		r := k.Apa(k.OBS, "", "source", "backlog", "restore", "--from", win.From, "--to", win.To, "--dry-run")
		if r.Code != 0 {
			t.Fatalf("预览退出码 %d\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		k.must(json.Unmarshal([]byte(r.Stdout), &cli))
		for name, p := range map[string]restorable{"CLI": cli, "HTTP": k.preview(q(""))} {
			if p.Matched != total || len(p.Sources) != len(bySource) {
				t.Fatalf("%s 预览 %+v，清单 %d / %v", name, p, total, bySource)
			}
			for _, s := range p.Sources {
				if s.Count != bySource[s.Source] {
					t.Fatalf("%s 预览来源 %s 为 %d，清单 %d", name, s.Source, s.Count, bySource[s.Source])
				}
			}
		}
		if len(k.inboundSince(seq0)) != 0 || len(k.eventRequests()) != 0 || len(stillIgnored()) != len(oldCascade)+len(controls) {
			for _, r := range k.modelLog() {
				t.Logf("MODEL 请求: %.600s", plain(r.BodyText))
			}
			t.Fatalf("预览写了处置 Event、调了模型或改了处置: 请求=%d ignored=%d want %d", len(k.eventRequests()), len(stillIgnored()), len(oldCascade)+len(controls))
		}
	})

	k.step("ACT-RST-04 预览按来源限定，参数错误被拒", func(t *testing.T) {
		if p := k.preview(q("&source=" + first)); p.Matched != bySource[first] {
			t.Fatalf("限定来源 %s: %+v，清单 %d", first, p, bySource[first])
		}
		for _, c := range []struct {
			query string
			want  int
		}{{"from=" + win.From + "&to=" + win.From, 400}, {"from=" + win.To + "&to=" + win.From, 400}, {"to=" + win.To, 400}, {q("&source=no-such-source"), 404}} {
			if r := k.APIRaw("GET", "/v1/source-events/restorable?"+c.query, "", k.OBS); r.Code != c.want {
				t.Fatalf("%s: HTTP %d，期望 %d\n%s", c.query, r.Code, c.want, r.Body)
			}
		}
	})

	k.step("ACT-RST-05 放回的授权边界", func(t *testing.T) {
		grantsBefore := k.maxSeq()
		// 授权单事先由本人在 Web 为夹具 delegate 签发：有效期内，动作勾选全部可选项。
		g, _ := json.Marshal(map[string]any{"grantee": map[string]string{"credential": "cred_fx_delegate"}, "scope": map[string]any{"kinds": []string{"task"}},
			"actions": policy.GrantableActions, "expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339), "reason": "e2e"})
		var grant struct{ ID string }
		k.must(json.Unmarshal([]byte(k.webOK("POST", "/v1/settings/grants", string(g), 201)), &grant))
		base := []string{"source", "backlog", "restore", "--from", win.From, "--to", win.To}
		if r := k.Apa(k.DEL, "", base...); r.Code != 23 {
			t.Fatalf("① 不带 --evidence 退出码 %d，期望 23\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		if r := k.Apa(k.DEL, "", append(base, "--grant", grant.ID)...); r.Code != 23 {
			t.Fatalf("② 带 --grant 不带 --evidence 退出码 %d，期望 23\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		r := k.run(k.DEL, []string{"APA_SESSION=e2e-session", "APA_SESSION_KIND=group"}, append(base, "--evidence", "本人：放回")...)
		if r.Code != 23 || !strings.Contains(r.Stdout+r.Stderr, "session_not_direct") {
			t.Fatalf("③ group 会话退出码 %d，期望 23 session_not_direct\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		if r := k.Apa(k.OBS, "", append(base, "--evidence", "本人：放回")...); r.Code != 4 {
			t.Fatalf("④ observer 退出码 %d，期望 4\n%s%s", r.Code, r.Stdout, r.Stderr)
		}
		run := k.runCredential()
		if r := k.APIRaw("GET", "/v1/health", "", run); r.Code != 200 { // 运行凭据此刻有效，下面的 403 才是权限而不是凭据失效
			t.Fatalf("运行凭据不可用: %d %s", r.Code, r.Body)
		}
		body := fmt.Sprintf(`{"from":%q,"to":%q}`, win.From, win.To)
		for name, resp := range map[string]int{
			"POST /v1/source-events/restore":   k.APIRaw("POST", "/v1/source-events/restore", body, run).Code,
			"GET /v1/source-events/restorable": k.APIRaw("GET", "/v1/source-events/restorable?"+q(""), "", run).Code,
		} {
			if resp != 403 {
				t.Fatalf("⑤ 运行凭据 %s 返回 %d，期望 403", name, resp)
			}
		}
		for _, e := range k.audit("inbound.disposed", grantsBefore) {
			t.Fatalf("被拒的尝试之后出现了处置 Event: %+v", e)
		}
		if p := k.preview(q("")); p.Matched != total {
			t.Fatalf("全部尝试后预览 matched=%d，应仍为 %d", p.Matched, total)
		}
		k.clear()
	})

	k.step("ACT-RST-06 按来源执行放回，不调模型、不建事项", func(t *testing.T) {
		seq, items := k.maxSeq(), len(k.proposed())
		var res struct{ Restored int }
		k.ownerOK("本人：把旧级联忽略的放回来", &res, "source", "backlog", "restore", "--from", win.From, "--to", win.To, "--source", first)
		if res.Restored != bySource[first] {
			t.Fatalf("restored=%d，该来源计数 %d", res.Restored, bySource[first])
		}
		restoredByA = res.Restored
		got := 0
		for _, e := range k.events(first, "unjudged", "") {
			if oldCascade[e.ID] {
				got++
				if e.Unjudged != "restored" {
					t.Fatalf("事件 %s 原因 %q，应为 restored", e.ID, e.Unjudged)
				}
			}
		}
		if got != res.Restored {
			t.Fatalf("该来源 unjudged/restored 的旧事件 %d 条，restored=%d", got, res.Restored)
		}
		per := map[string]int{}
		for _, d := range k.audit("inbound.disposed", seq) {
			per[d.Object]++
			if d.Actor.Principal != "cred_fx_delegate" {
				t.Fatalf("操作者应是该 delegate: %+v", d.Actor)
			}
		}
		if len(per) != res.Restored {
			t.Fatalf("inbound.disposed 覆盖 %d 条事件，restored=%d", len(per), res.Restored)
		}
		for id, n := range per {
			if n != 1 || !oldCascade[id] {
				t.Fatalf("事件 %s 有 %d 条 inbound.disposed（旧事件=%v）", id, n, oldCascade[id])
			}
		}
		// 载荷含原处置 ignored 与本次 from、to：载荷只对本人（Web）可见。
		var page struct {
			Events []struct{ Payload json.RawMessage }
		}
		k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/audit/events?type=inbound.disposed&limit=500&after="+fmt.Sprint(seq), "", 200)), &page))
		for _, e := range page.Events {
			if p := string(e.Payload); !strings.Contains(p, "ignored") || !strings.Contains(p, win.From[:10]) || !strings.Contains(p, win.To[:10]) {
				t.Fatalf("inbound.disposed 载荷缺原处置或 from/to: %s", p)
			}
		}
		if len(k.eventRequests()) != 0 || len(k.proposed()) != items {
			t.Fatal("放回不应调模型或建事项")
		}
		if n := len(k.audit("item.created", seq)); n != 0 {
			t.Fatalf("放回期间出现 %d 条 item.created", n)
		}
	})

	k.step("ACT-RST-07 本人在 Web 来源页先预览再放回其余来源", func(t *testing.T) {
		var pv restorable
		k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/source-events/restorable?"+q(""), "", 200)), &pv))
		if pv.Matched != total-restoredByA {
			t.Fatalf("Web 预览 %d 条，期望 %d", pv.Matched, total-restoredByA)
		}
		seq := k.maxSeq()
		var res struct{ Restored int }
		k.must(json.Unmarshal([]byte(k.webOK("POST", "/v1/source-events/restore", fmt.Sprintf(`{"from":%q,"to":%q}`, win.From, win.To), 200)), &res))
		if res.Restored != pv.Matched {
			t.Fatalf("执行放回 %d 条，预览 %d 条", res.Restored, pv.Matched)
		}
		ds := k.audit("inbound.disposed", seq)
		if len(ds) != res.Restored {
			t.Fatalf("inbound.disposed %d 条，restored=%d", len(ds), res.Restored)
		}
		for _, d := range ds {
			if d.Actor.Principal != "owner" {
				t.Fatalf("操作者应是本人: %+v", d.Actor)
			}
		}
	})

	k.step("ACT-RST-08 对照组与窗口外的事件不被放回", func(t *testing.T) {
		ign := stillIgnored()
		for _, id := range controls {
			if !ign[id] {
				t.Fatalf("对照组事件 %s 不再是 ignored", id)
			}
		}
		if len(ign) != len(controls) {
			t.Fatalf("仍为 ignored 的事件 %d 条，应只剩对照组 %d 条", len(ign), len(controls))
		}
		for _, d := range k.audit("inbound.disposed", seq0) {
			if slices.Contains(controls, d.Object) {
				t.Fatalf("对照组事件 %s 出现了新的 inbound.disposed", d.Object)
			}
		}
	})

	k.step("ACT-RST-09 同一时间段第二次执行放回 0 条", func(t *testing.T) {
		seq := k.maxSeq()
		var res struct{ Restored int }
		k.ownerOK("本人：把旧级联忽略的放回来", &res, "source", "backlog", "restore", "--from", win.From, "--to", win.To)
		if res.Restored != 0 {
			t.Fatalf("CLI 第二次放回 %d 条", res.Restored)
		}
		for i := 0; i < 2; i++ {
			r := k.restoreHTTP("same-key", win.From, win.To)
			if r.Code != 200 || !strings.Contains(r.Body, `"restored":0`) {
				t.Fatalf("带幂等键第 %d 次: %d %s", i+1, r.Code, r.Body)
			}
		}
		if ev := k.inboundSince(seq); len(ev) != 0 {
			t.Fatalf("重复放回写了新的处置 Event: %+v", ev)
		}
	})

	k.step("ACT-RST-10 健康视图计入 restored", func(t *testing.T) {
		_, d, signals := k.unjudgedHealth()
		if num(d, "restored") != total || signals != 13 {
			t.Fatalf("detail.restored=%d（期望 %d），信号 %d 项", num(d, "restored"), total, signals)
		}
	})

	k.step("ACT-RST-11 restored 的事件不被自动重判也不转 expired", func(t *testing.T) {
		seq := k.maxSeq()
		k.StopKernel()
		k.setConfig("unjudged_rejudge_days", "7")
		k.StartKernelAgain()
		k.TCAdvance(1555200) // T0+18d：全部 restored 事件都早于 7 天窗口
		k.TCAdvance(900)     // 等待 15 分钟
		time.Sleep(3 * time.Second)
		for _, e := range k.all("unjudged,expired") {
			if oldCascade[e.ID] && (e.Disposition != "unjudged" || e.Unjudged != "restored") {
				t.Fatalf("restored 事件 %s 变成 %s/%s", e.ID, e.Disposition, e.Unjudged)
			}
		}
		for _, r := range k.audit("inbound.requeued", seq) {
			if r.Actor.Principal == "kernel:inbound" {
				t.Fatalf("内核自动重判了事件 %s", r.Object)
			}
		}
		for _, b := range k.clsLog() {
			if strings.Contains(plain(b.BodyText), "Old cascade ignored") {
				t.Fatal("判断端点收到了 restored 事件的判断题")
			}
		}
		for _, r := range k.judgeRequests() {
			if strings.Contains(plain(r.BodyText), "Old cascade ignored") {
				t.Fatal("对话端点收到了 restored 事件的判断题")
			}
		}
	})

	k.step("ACT-RST-12 restored 的事件只经四步重判并受每日上限约束", func(t *testing.T) {
		k.StopKernel()
		k.setConfig("unjudged_rejudge_days", "0")
		k.StartKernelAgain()
		k.clear()
		seq12 := k.maxSeq()
		var rules struct{ Attached, Disposed, Remaining int }
		k.ownerOK("本人：先只跑规则", &rules, "source", "backlog", "rules")
		if len(k.eventRequests()) != 0 {
			t.Fatal("rules 不应调模型")
		}
		var score struct{ Scored, Skipped int }
		k.ownerOK("本人：影子打分", &score, "source", "backlog", "score")
		shadows := 0
		for _, e := range k.backlog() {
			if !oldCascade[e.ID] {
				continue
			}
			js := k.judgmentsOf(e.ID)
			if len(js) == 0 || !js[len(js)-1].Shadow {
				t.Fatalf("restored 事件 %s 缺影子判断记录", e.ID)
			}
			shadows++
		}
		if shadows == 0 {
			t.Fatal("score 没有给 restored 事件写判断记录")
		}
		before := k.proposed() // 第 1 步规则直接处置的建议不走放开重判，不计入每日上限
		res := k.rejudge()
		if got := len(k.freshProposals(before)); got > perDay || got == 0 {
			t.Fatalf("当天经放开重判进入建议流 %d 条，应为 1..%d", got, perDay)
		}
		// 放回的事件分数最高（0.95），先被放回重判；它们的 triage-screen 题经判断端点按批提交，没有绕过初筛。
		seqNow := k.maxSeq()
		_ = seqNow
		asked := 0
		for _, b := range k.clsLog() {
			asked += strings.Count(plain(b.BodyText), "Old cascade ignored")
		}
		requeued := 0
		for _, r := range k.audit("inbound.requeued", seq12) {
			if !oldCascade[r.Object] {
				t.Fatalf("按分数从高到低，最先放回的应是 restored 事件，却是 %s", r.Object)
			}
			requeued++
		}
		if asked == 0 || requeued != res.Requeued {
			t.Fatalf("判断端点收到 restored 事件的题 %d 道；放回 %d 条（requeued=%d）", asked, requeued, res.Requeued)
		}
		if res.Deferred == 0 && len(k.freshProposals(before)) == perDay {
			t.Fatalf("建议已达上限却没有 deferred: %+v", res)
		}
	})

	k.step("ACT-RST-13 重判后再被忽略的事件不再可放回", func(t *testing.T) {
		for day := 0; day < 60; day++ {
			if len(k.backlog()) == 0 {
				break
			}
			k.TCAdvance(86400)
			if res := k.rejudge(); res.Deferred == 0 {
				break
			}
		}
		if left := len(k.backlog()); left != 0 {
			t.Fatalf("积压没有放完: 剩 %d", left)
		}
		if p := k.preview(q("")); p.Matched != 0 {
			t.Fatalf("经过新级联的事件仍可被放回: %+v", p)
		}
		if _, d, _ := k.unjudgedHealth(); num(d, "restored") != 0 {
			t.Fatalf("detail.restored=%d，应为 0", num(d, "restored"))
		}
		ign := 0
		for _, e := range k.all("ignored") {
			if oldCascade[e.ID] {
				ign++
				if _, ok := k.decision(e.ID); !ok {
					t.Fatalf("新级联忽略的事件 %s 没有判断记录", e.ID)
				}
			}
		}
		if ign == 0 {
			t.Fatal("没有旧事件被新级联忽略，不能证明'重判后再被忽略不再命中'")
		}
	})
}

// restoreHTTP 以本人原话调 POST /v1/source-events/restore，带幂等键。
func (k *kit) restoreHTTP(idem, from, to string) (r struct {
	Code int
	Body string
}) {
	k.t.Helper()
	resp := k.APIRawHeaders("POST", "/v1/source-events/restore", fmt.Sprintf(`{"from":%q,"to":%q}`, from, to), k.DEL, map[string]string{
		"X-APA-Session": "e2e-session", "X-APA-Session-Kind": "direct", "X-APA-Evidence": "本人：把旧级联忽略的放回来", "Idempotency-Key": idem})
	r.Code, r.Body = resp.Code, resp.Body
	return
}
