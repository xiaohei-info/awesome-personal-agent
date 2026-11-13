//go:build e2e

package followup

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

const (
	ruleKey = "follow-up/clock_rules/follow-up-interval"
	// settle 是一次时钟推进后等内核落地的上限。
	settle = 20 * time.Second
)

// shortPack 把内置 follow-up 包复制到 $APA_HOME/packs/follow-up，只把 follow-up-interval 的 schedule 改为 1m，
// 可选再用 edit 改一次文本（ACT-FUP-01 / ACT-FUP-12 / ACT-FUP-15）。
func (f *fx) shortPack(edit func(string) string) {
	f.t.Helper()
	dst := filepath.Join(f.Home, "packs", "follow-up")
	_ = os.RemoveAll(dst)
	if err := os.CopyFS(dst, os.DirFS("../../../packs/follow-up")); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dst, "pack.yaml")
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "schedule: 72h") {
		f.t.Fatalf("内置包没有 schedule: 72h: %v", err)
	}
	s := strings.Replace(string(b), "schedule: 72h", "schedule: 1m", 1)
	if edit != nil {
		s = edit(s)
	}
	writeFile(f.t, p, s)
}

// schedule 是当前生效的 follow-up 包里 follow-up-interval 的 schedule（GET /v1/packs）。
func (f *fx) schedule() string {
	f.t.Helper()
	for _, p := range decode[struct {
		Packs []struct {
			Name       string                          `json:"name"`
			ClockRules []struct{ ID, Schedule string } `json:"clock_rules"`
		} `json:"packs"`
	}](f.t, f.webOK("GET", "/packs", "", 200)).Packs {
		for _, c := range p.ClockRules {
			if p.Name == "follow-up" && c.ID == "follow-up-interval" {
				return c.Schedule
			}
		}
	}
	return ""
}

// reminders 取 Web 未读里指向该事项、带三个动作的跟进提醒数量（用来数"恰有几条"）。
func (f *fx) reminders(id string) int { return len(f.followupDeliveries(id)) }

// advanceTo 每次把测试时钟推进 sec 秒，直到事项的 followup.reminded 达到 n。
func (f *fx) advanceUntilReminded(id string, n int) {
	f.t.Helper()
	f.TCAdvance(65)
	waitFor(f.t, id+" 的跟进提醒应达到 "+string(rune('0'+n))+" 条", settle, func() bool { return f.reminded(id) >= n })
}

// TestFlowFollowUp 对应 flow-follow-up（ACT-FUP-01..12、15）；升级链路 ACT-FUP-13、14 在 TestFlowFollowUpUpgrade。
// 跟进间隔 1 分钟，用 baseline 的测试时钟推进，不靠真实等待；时钟只进不退，各事项按各自 Timer 的到期时刻互不影响。
func TestFlowFollowUp(t *testing.T) {
	f := newFx(t)
	f.WireEndpoints()
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false}}`, 200)
	var clsBefore, modelBefore int

	step(t, "ACT-FUP-01 缩短跟进间隔并停掉模型替身", func(t *testing.T) {
		f.shortPack(nil)
		waitFor(t, "follow-up 包按 1m 覆盖生效", 30*time.Second, func() bool {
			return f.schedule() == "1m"
		})
		if r := f.rules()["follow-up-interval"]; r.Mode != "auto" || r.Key != ruleKey {
			t.Fatalf("follow-up-interval 应是 auto: %+v", r)
		}
		if n := len(f.events("", "rule.mode_changed")); n != 0 {
			t.Fatalf("不应为此切换过模式，审计里有 %d 条 rule.mode_changed", n)
		}
		clsBefore, modelBefore = len(f.Cls.Batches()), len(f.ModelStub.Requests())
		f.Cls.Close()
		f.ModelStub.Close()
	})

	var fu1 string
	step(t, "ACT-FUP-02 第一轮到期提醒本人并带三个动作，不调模型", func(t *testing.T) {
		fu1 = f.newItem("FU-1")
		f.wait(fu1)
		f.advanceUntilReminded(fu1, 1)
		ds := f.followupDeliveries(fu1)
		if len(ds) != 1 {
			t.Fatalf("应恰有一条跟进提醒，实际 %d: %+v", len(ds), f.unread())
		}
		if got := strings.Join(ds[0].Content.Options, "/"); got != "已催/再等/放弃" {
			t.Errorf("提醒的三个动作 = %q", got)
		}
		if p := f.show(fu1).Phase; p != "waiting" {
			t.Errorf("FU-1 阶段 = %s，期望 waiting", p)
		}
		if len(f.Cls.Batches()) != clsBefore || len(f.ModelStub.Requests()) != modelBefore {
			t.Errorf("跟进不得调用模型: CLS %d→%d MODEL %d→%d", clsBefore, len(f.Cls.Batches()), modelBefore, len(f.ModelStub.Requests()))
		}
	})

	step(t, "ACT-FUP-03 选已催：记一条进展并排下一轮", func(t *testing.T) {
		before := len(f.show(fu1).Progress)
		f.webOK("POST", "/items/"+fu1+"/progress", `{"text":"已催：电话问过装修公司"}`, 201)
		it := f.show(fu1)
		if len(it.Progress) != before+1 || it.Phase != "waiting" {
			t.Fatalf("已催应新增一条进展且仍 waiting: progress %d→%d phase=%s", before, len(it.Progress), it.Phase)
		}
		f.TCAdvance(30)
		quiet(t, "下一轮不应在约 1 分钟之前出现", 2*time.Second, func() bool { return f.reminded(fu1) == 1 })
		f.advanceUntilReminded(fu1, 2)
	})

	step(t, "ACT-FUP-04 选再等：只重排，不记进展", func(t *testing.T) {
		fu2 := f.newItem("FU-2")
		f.wait(fu2)
		f.advanceUntilReminded(fu2, 1)
		before := len(f.show(fu2).Progress)
		f.advanceUntilReminded(fu2, 2) // 再等什么都不用做，下一轮在提醒产出时就已排好
		if it := f.show(fu2); len(it.Progress) != before || it.Phase != "waiting" {
			t.Fatalf("再等不应记进展或改阶段: progress %d→%d phase=%s", before, len(it.Progress), it.Phase)
		}
	})

	step(t, "ACT-FUP-05 选放弃：事项转 dropped 且不再提醒", func(t *testing.T) {
		fu3 := f.newItem("FU-3")
		f.wait(fu3)
		f.advanceUntilReminded(fu3, 1)
		f.webOK("POST", "/items/"+fu3+"/transition", `{"to":"dropped","reason":"不等了"}`, 200)
		if it := f.show(fu3); it.Phase != "dropped" {
			t.Fatalf("FU-3 阶段 = %s，期望 dropped", it.Phase)
		}
		var ev []auditEvent
		for _, e := range f.events(fu3, "item.phase_changed") {
			ev = append(ev, e)
		}
		if last := ev[len(ev)-1]; last.Actor.Via != "web" {
			t.Errorf("放弃的操作者应是本人（Web）: %+v", last.Actor)
		}
		for i := 0; i < 3; i++ {
			f.TCAdvance(65)
		}
		time.Sleep(2 * time.Second)
		if f.reminded(fu3) != 1 {
			t.Errorf("放弃之后不应再有跟进提醒，followup.reminded = %d", f.reminded(fu3))
		}
	})

	step(t, "ACT-FUP-06 连续 remind_rounds 轮没有进展后转 needs_input 问去留", func(t *testing.T) {
		fu4 := f.newItem("FU-4")
		f.wait(fu4)
		f.advanceUntilReminded(fu4, 1)
		f.advanceUntilReminded(fu4, 2)
		f.TCAdvance(65)
		waitFor(t, "第三次到期转 needs_input", settle, func() bool { return f.show(fu4).Phase == "needs_input" })
		if n := f.reminded(fu4); n != 2 {
			t.Errorf("前两轮各恰有一条提醒，不应有第三条: followup.reminded = %d", n)
		}
		inbox := f.apa(f.OBS, 0, "inbox").Stdout
		if !strings.Contains(inbox, fu4) {
			t.Fatalf("决策待办里没有 FU-4: %s", inbox)
		}
		for _, label := range []string{"继续等", "我来处理", "放弃"} {
			if raw := f.apa(f.OBS, 0, "item", "show", fu4).Stdout; !strings.Contains(raw, label) {
				t.Errorf("决策请求缺少选项 %q: %s", label, raw)
			}
		}
		if f.show(fu4).Phase == "dropped" {
			t.Error("FU-4 不得被自动 dropped")
		}
	})

	step(t, "ACT-FUP-07 新进展让轮次清零", func(t *testing.T) {
		fu5 := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.WRK, 0, "item", "add", "--kind", "task", "--title", "FU-5", "--phase", "open", "--assignee", "work-host.cli",
			"--project", f.Projects["acc-work"], "--evidence", "WRK 建事项").Stdout).ID
		f.wait(fu5)
		f.advanceUntilReminded(fu5, 1)
		f.apa(f.WRK, 0, "item", "progress", fu5, "--text", "对方说周五给", "--evidence", "对方说周五给")
		f.advanceUntilReminded(fu5, 2)
		f.advanceUntilReminded(fu5, 3)
		f.TCAdvance(65)
		waitFor(t, "三轮提醒之后才转 needs_input", settle, func() bool { return f.show(fu5).Phase == "needs_input" })
		if n := f.reminded(fu5); n != 3 {
			t.Errorf("写进展之后还应出现两轮提醒（共三条），实际 %d 条", n)
		}
	})

	step(t, "ACT-FUP-08 委派迟迟不回执时跟进", func(t *testing.T) {
		fu6 := f.newItem("FU-6")
		child := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "delegate", "--parent", fu6, "--assignee", "work-host.cli", "--title", "FU-6 子事项", "--reason", "委派", "--evidence", "owner asked").Stdout).ID
		if p := f.show(fu6).Phase; p != "waiting" {
			t.Fatalf("委派后父事项应进入 waiting（等执行者），实际 %s", p)
		}
		f.advanceUntilReminded(fu6, 1)
		ds := f.followupDeliveries(fu6)
		if len(ds) != 1 || strings.Join(ds[0].Content.Options, "/") != "已催/再等/放弃" {
			t.Fatalf("应有一条带三个动作的跟进提醒: %+v", ds)
		}
		if p := f.show(fu6).Phase; p != "waiting" {
			t.Errorf("父事项阶段 = %s，期望 waiting", p)
		}
		if p := f.show(child).Phase; p != "open" {
			t.Errorf("子事项被改动: %s", p)
		}
	})

	deferCalls := func() (n int) {
		for _, b := range f.Cls.Batches() {
			if strings.Contains(b.BodyText, "可以晚点提") {
				n++
			}
		}
		return n
	}
	policyDefer := func() bool {
		return decode[struct {
			Disturb struct {
				FollowupDefer bool `json:"followup_defer"`
			} `json:"disturb"`
		}](t, f.webOK("GET", "/settings/policy", "", 200)).Disturb.FollowupDefer
	}
	setDefer := func(on bool) {
		t.Helper()
		v := "false"
		if on {
			v = "true"
		}
		f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false,"followup_defer":`+v+`}}`, 200)
		if policyDefer() != on {
			t.Fatalf("disturb.followup_defer 应为 %v", on)
		}
	}

	step(t, "ACT-FUP-09 推迟判断默认关闭", func(t *testing.T) {
		if policyDefer() {
			t.Fatal("disturb.followup_defer 默认应为 false")
		}
		f.Cls, f.ModelStub = nil, nil
		f.WireEndpoints() // 按 ACT-ENV-04、05 恢复两个替身并接线
		f.Cls.SetDefaultPYes(0.95)
		fu7 := f.newItem("FU-7")
		f.wait(fu7)
		f.advanceUntilReminded(fu7, 1)
		if f.reminders(fu7) != 1 {
			t.Errorf("FU-7 应照常收到第一轮提醒: %+v", f.unread())
		}
		if n := deferCalls(); n != 0 {
			t.Errorf("未开启时不得问推迟判断，CLS 收到 %d 道题", n)
		}
	})

	step(t, "ACT-FUP-10 开启推迟判断后至多连续跳过一轮，且不替本人选动作", func(t *testing.T) {
		setDefer(true)
		fu8 := f.newItem("FU-8")
		f.wait(fu8)
		before := len(f.show(fu8).Progress)
		f.TCAdvance(65)
		waitFor(t, "第一次到期应问一道推迟判断并跳过一轮", settle, func() bool { return len(f.events(fu8, "followup.deferred")) == 1 })
		if n := deferCalls(); n < 1 {
			t.Error("第一次到期 CLS_LOG 应有一道 follow-up-defer 题")
		}
		if f.reminded(fu8) != 0 || f.reminders(fu8) != 0 {
			t.Error("判断为可以晚点时第一轮不应提醒")
		}
		f.advanceUntilReminded(fu8, 1) // 第二次到期即使判断仍为"可以晚点"也照常提醒
		it := f.show(fu8)
		if it.Phase != "waiting" || len(it.Progress) != before {
			t.Errorf("判断不得改变事项状态: phase=%s progress %d→%d", it.Phase, before, len(it.Progress))
		}
		if len(f.events(fu8, "followup.deferred")) != 1 {
			t.Error("至多连续跳过一轮")
		}
		if out := f.apa(f.OBS, 0, "judgment", "stats", "--use", "followup_defer").Stdout; !strings.Contains(out, "followup_defer") {
			t.Errorf("判断记录应可见用途 followup_defer: %s", out)
		}
	})

	step(t, "ACT-FUP-11 推迟判断低于上限或端点不可用时照常提醒", func(t *testing.T) {
		f.Cls.SetDefaultPYes(0.50)
		fu9 := f.newItem("FU-9")
		f.wait(fu9)
		f.advanceUntilReminded(fu9, 1)
		if f.reminders(fu9) != 1 || len(f.events(fu9, "followup.deferred")) != 0 {
			t.Errorf("0.50 低于上限 0.85 取否：FU-9 应按时收到第一轮提醒且不被跳过: %+v", f.unread())
		}
		f.Cls.Close()
		f.ModelStub.Close()
		fu10 := f.newItem("FU-10")
		f.wait(fu10)
		f.advanceUntilReminded(fu10, 1)
		if f.reminders(fu10) != 1 || len(f.events(fu10, "followup.deferred")) != 0 {
			t.Errorf("端点不可用时未定也取否：FU-10 应按时收到第一轮提醒且不被跳过: %+v", f.unread())
		}
		waitFor(t, "端点不可用时健康视图 model_unavailable 有信号", settle, func() bool {
			for _, s := range decode[struct {
				Signals []struct {
					ID string `json:"id"`
					OK bool   `json:"ok"`
				} `json:"signals"`
			}](t, f.apa(f.OBS, 0, "health").Stdout).Signals {
				if s.ID == "model_unavailable" {
					return !s.OK
				}
			}
			return false
		})
	})

	step(t, "ACT-FUP-12 覆盖包里 follow_up 规则带 do[] 被拒绝", func(t *testing.T) {
		setDefer(false)
		f.Cls, f.ModelStub = nil, nil
		f.WireEndpoints()
		f.shortPack(func(s string) string {
			return strings.Replace(s, "remind_rounds: 2", "remind_rounds: 2\n    do: [{call_agent: follow-up}]", 1)
		})
		waitFor(t, "健康视图 pack_rejections 指出 follow-up-interval 带 do[]", 30*time.Second, func() bool {
			h := f.apa(f.OBS, 0, "health").Stdout
			return strings.Contains(h, "pack_rejections") && strings.Contains(h, "do[]")
		})
		if doc := f.Apa(f.OBS, "", "doctor"); !strings.Contains(doc.Stdout+doc.Stderr, "do[]") {
			t.Errorf("apa doctor 应指出 follow-up-interval 带 do[] 并提示删除: %s%s", doc.Stdout, doc.Stderr)
		}
		// 设计（附录 C.7）：被拒绝的包保留上一有效版本，所以热加载中途改坏时仍是 1m 的覆盖版，跟进没有因此停摆。
		if got := f.schedule(); got != "1m" {
			t.Errorf("热加载被拒后应保留上一有效版本（1m），实际 %q", got)
		}
		// 没有上一有效版本时（内核重启后第一次加载就是这份坏包）同名内置包生效：follow-up-interval 回到内置定义。
		f.StopKernel()
		f.StartKernelAgain()
		f.WebLogin()
		if got := f.schedule(); got != "72h" {
			t.Errorf("重启后坏包被拒，follow-up-interval 应回到内置定义（72h），实际 %q", got)
		}
		if h := f.apa(f.OBS, 0, "health").Stdout; !strings.Contains(h, "do[]") {
			t.Errorf("重启后健康视图仍应指出 do[]: %s", h)
		}
		if f.rules()["follow-up-interval"].Mode != "auto" {
			t.Error("拒绝后内置 follow_up 规则应生效（auto）")
		}
	})

	step(t, "ACT-FUP-15 恢复跟进配置", func(t *testing.T) {
		if err := os.RemoveAll(filepath.Join(f.Home, "packs", "follow-up")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "覆盖残留被清除", 30*time.Second, func() bool {
			h := f.apa(f.OBS, 0, "health").Stdout
			return !strings.Contains(h, "do[]") && f.schedule() == "72h"
		})
		if r := f.rules()["follow-up-interval"]; r.Mode != "auto" {
			t.Errorf("follow-up-interval 应为内置定义的 auto: %+v", r)
		}
		if policyDefer() {
			t.Error("followup_defer 应为关闭")
		}
	})
}

// timerAt 读账本里事项的跟进 Timer 到期时刻（item show 不外露 Timer，验收读 clock_timer 表）。
func timerAt(t *testing.T, home, item string) time.Time {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var at int64
	if err := db.QueryRow(`SELECT at FROM clock_timer WHERE id = ?`, "followup_"+item).Scan(&at); err != nil {
		t.Fatalf("读 %s 的跟进 Timer: %v", item, err)
	}
	return time.Unix(0, at).UTC()
}

// TestFlowFollowUpUpgrade 对应 ACT-FUP-13、14：v0.1 夹具副本上排一个即将到期的跟进定时，升级后定时不变，到期按新梯度处理。
func TestFlowFollowUpUpgrade(t *testing.T) {
	e := baseline.New(t)
	fx := e.LedgerFixture(2) // FX_F
	const fuX = "itm_fx_prop_01"
	var due time.Time

	step(t, "ACT-FUP-13 在 v0.1 夹具副本上排一个即将到期的跟进定时", func(t *testing.T) {
		// v0.1 写法的覆盖包：follow_up 规则 schedule 4m，带 v0.1 的 do[]。
		p := filepath.Join(fx.Home, "packs", "follow-up", "pack.yaml")
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), "schedule: 72h") || !strings.Contains(string(b), "do:") {
			t.Fatalf("夹具的 follow-up 用户包应是 v0.1 写法: %v", err)
		}
		writeFile(t, p, strings.Replace(string(b), "schedule: 72h", "schedule: 4m", 1))
		v := startInst(t, e.V01Bin(), fx.Home, "")
		v.login(fx.M.WebSecret)
		if code, body := v.web("PATCH", "/v1"+modePath(ruleKey), `{"mode":"auto"}`); code != 200 {
			t.Fatalf("v0.1 切 auto: %d %s", code, body)
		}
		if code, body := v.web("POST", "/v1/items/"+fuX+"/transition", `{"to":"waiting","reason":"FU-X","waiting_on":{"kind":"external","ref":"vendor"}}`); code != 200 {
			t.Fatalf("FU-X 转 waiting: %d %s", code, body)
		}
		v.stop()
		due = timerAt(t, fx.Home, fuX)
		if d := time.Until(due); d < 3*time.Minute || d > 5*time.Minute {
			t.Fatalf("T 应在当前时间约 4 分钟之后，实际 %s", d)
		}
	})

	step(t, "ACT-FUP-14 升级后已排的跟进定时不变，到期按新梯度处理", func(t *testing.T) {
		cls, model := modelstub.NewClsAt("127.0.0.1:0"), modelstub.NewAt("127.0.0.1:0")
		defer cls.Close()
		defer model.Close()
		n := startInst(t, e.Bin["apa"], fx.Home, "")
		n.login(fx.M.WebSecret)
		endpoints, _ := json.Marshal(map[string]any{
			"judge_endpoint":       map[string]any{"url": cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1", "locality": "local"},
			"generate_endpoint":    map[string]any{"url": model.URL(), "api_key": "stub-key", "model": "acc-model", "locality": "local"},
			"screening_timeout_ms": 2000,
		})
		must := func(code int, body string) {
			t.Helper()
			if code != 200 {
				t.Fatalf("Web 设置: %d %s", code, body)
			}
		}
		must(n.web("PUT", "/v1/settings/model-endpoints", string(endpoints)))
		must(n.web("PUT", "/v1/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false}}`))

		tok := fx.M.Tokens["observer"]
		code, health := n.apa(tok, "health")
		if code != 0 || !strings.Contains(health, "pack_rejections") || !strings.Contains(health, "do[]") {
			t.Fatalf("v0.1 覆盖包应被拒（pack_rejections 提示删除 do[]）: %d %s", code, health)
		}
		if _, body := n.api("GET", "/v1/rules", "", tok); !strings.Contains(body, `"mode":"auto"`) {
			t.Fatalf("内置 follow-up 应生效: %s", body)
		}
		if got := timerAt(t, fx.Home, fuX); !got.Equal(due) {
			t.Fatalf("FU-X 的跟进 Timer 到期时刻应仍为 T=%s，实际 %s", due, got)
		}

		// 推进测试时钟到 T 之后 1 分钟。
		off := int64(time.Until(due).Seconds()) + 60
		writeFile(t, filepath.Join(fx.Home, "test-clock-offset"), strconv.FormatInt(off, 10)+"\n")
		waitFor(t, "T 到期后应产出一条带三个动作的提醒", 30*time.Second, func() bool {
			_, body := n.web("GET", "/v1/deliveries/unread", "")
			for _, d := range decode[struct {
				Deliveries []delivery `json:"deliveries"`
			}](t, body).Deliveries {
				if d.Item == fuX && d.Class == "reminder" && strings.Join(d.Content.Options, "/") == "已催/再等/放弃" {
					return true
				}
			}
			return false
		})
		for _, r := range model.Requests() {
			// 事项转 waiting 时 decision-card 包起草的一句话摘要是另一个模板的外呼，不是跟进判断。
			if strings.Contains(r.BodyText, fuX) && !strings.Contains(r.BodyText, "一句话摘要") {
				t.Errorf("MODEL_STUB 里不应有为 FU-X 发出的跟进判断请求: %s", r.BodyText)
			}
		}
		for _, b := range cls.Batches() {
			if strings.Contains(b.BodyText, fuX) {
				t.Errorf("CLS_STUB 里不应有为 FU-X 发出的判断请求: %s", b.BodyText)
			}
		}
		next := timerAt(t, fx.Home, fuX)
		if d := next.Sub(due); d < 72*time.Hour || d > 72*time.Hour+10*time.Minute {
			t.Errorf("下一轮应按内置间隔 72h 排，距 T %s", d)
		}
	})
}
