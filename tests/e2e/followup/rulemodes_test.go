//go:build e2e

package followup

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const accTagPack = `name: acc-rule
version: 1.0.0
description: 规则模式验收
event_rules:
  - id: acc-tag
    when:
      event: item.phase_changed
      to_phase: open
      tag: [acc-rule]
    do:
      - action: add_tag
        tag: acc-hit
`

const accLoopPack = `name: acc-loop
version: 1.0.0
description: 规则动作不再触发规则
event_rules:
  - id: loop-a
    when:
      tag: acc-loop
    do:
      - action: set_phase
        phase: waiting
  - id: loop-b
    when:
      event: item.phase_changed
    do:
      - action: add_tag
        tag: acc-b
`

// accCatPack 对应 ACT-RULE-10：类别由加载器推定，能力包不能自报。withAuto 为真时 cat-follow 写了 mode: auto。
func accCatPack(withAuto bool) string {
	auto := ""
	if withAuto {
		auto = "    mode: auto\n"
	}
	return `name: acc-cat
version: 1.0.0
description: 规则类别由加载器推定
event_rules:
  - id: cat-timer
    when:
      event: item.created
      tag: [acc-cat]
    do:
      - action: create_timer
        after: 1m
clock_rules:
  - id: cat-stale
    type: stale
    schedule: 24h
  - id: cat-follow
    type: follow_up
    schedule: 72h
` + auto
}

func (f *fx) packDir(name string) string { return filepath.Join(f.Home, "packs", name) }

// ---- 本流程内的辅助 ----

func (f *fx) openItem(tag string) string {
	f.t.Helper()
	id := decode[struct {
		ID string `json:"id"`
	}](f.t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "rule-"+tag, "--tags", tag, "--reason", "rule e2e", "--evidence", "rule e2e").Stdout).ID
	f.apa(f.DEL, 0, "item", "transition", id, "--to", "open", "--reason", "rule e2e", "--evidence", "rule e2e")
	return id
}

func (f *fx) why(id string) string { f.t.Helper(); return f.apa(f.DEL, 0, "why", id).Stdout }

func (f *fx) healthDeliveries() (n int) {
	for _, d := range f.unread() {
		if d.Class == "health" {
			n++
		}
	}
	return n
}

func (f *fx) healthText() string { f.t.Helper(); return f.apa(f.OBS, 0, "health").Stdout }

// TestFlowRuleModes 对应 flow-rule-modes（ACT-RULE-01..16）：
// 主实例验证动作类规则 observe → Web 切 auto → off、内容变化回 observe、类别推定、提醒类内容变化后保持选择；
// 全新安装的实例验证两类初始值与截止提醒开箱生效；三份 v0.1 结构夹具验证升级时默认值切换并留痕。
func TestFlowRuleModes(t *testing.T) {
	f := newFx(t)
	const tagKey = "acc-rule/event_rules/acc-tag"

	step(t, "ACT-RULE-01 准备动作类规则", func(t *testing.T) {
		writeFile(t, filepath.Join(f.packDir("acc-rule"), "pack.yaml"), accTagPack)
		waitFor(t, "GET /v1/rules 应显示 acc-tag", 30*time.Second, func() bool { _, ok := f.rules()["acc-tag"]; return ok })
		if s := f.rules()["acc-tag"]; s.Mode != "observe" || s.Key != tagKey {
			t.Fatalf("动作类规则缺省即 observe: %+v", s)
		}
	})

	step(t, "ACT-RULE-02 observe 只记录本来会做什么", func(t *testing.T) {
		id := f.openItem("acc-rule")
		waitFor(t, "apa why 应出现 rule.observed", 30*time.Second, func() bool { return strings.Contains(f.why(id), "rule.observed") })
		w := f.why(id)
		if !strings.Contains(w, tagKey) || strings.Contains(w, "outcall") {
			t.Errorf("rule.observed 应指向 %s 且无外呼: %s", tagKey, w)
		}
		if f.show(id).has("acc-hit") {
			t.Error("observe 不得改业务状态")
		}
	})

	step(t, "ACT-RULE-03 只在 Web 切换到 auto", func(t *testing.T) {
		if r := f.APIRaw("PATCH", "/v1"+modePath(tagKey), `{"mode":"auto"}`, f.DEL); r.Code != 403 {
			t.Errorf("token 切 auto 应 403，实际 %d %s", r.Code, r.Body)
		}
		if f.rules()["acc-tag"].Mode != "observe" {
			t.Error("被拒后模式应不变")
		}
		f.webOK("PATCH", modePath(tagKey), `{"mode":"auto"}`, 200)
		if f.rules()["acc-tag"].Mode != "auto" {
			t.Error("Web 切换后应为 auto")
		}
		evs := f.events("rule:"+tagKey, "rule.mode_changed")
		if len(evs) != 1 || evs[0].Actor.Type != "human" || evs[0].Actor.Via != "web" {
			t.Errorf("事件流应有一条本人（Web）的 rule.mode_changed: %+v", evs)
		}
	})

	step(t, "ACT-RULE-04 auto 时执行动作并记授权依据", func(t *testing.T) {
		id := f.openItem("acc-rule")
		waitFor(t, "auto 时事项应有 acc-hit", 30*time.Second, func() bool { return f.show(id).has("acc-hit") })
		var tl struct {
			Steps []struct {
				Basis *struct {
					PolicyVersion *int   `json:"policy_version"`
					RuleID        string `json:"rule_id"`
				} `json:"basis"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(f.why(id)), &tl); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range tl.Steps {
			found = found || (s.Basis != nil && s.Basis.RuleID == tagKey && s.Basis.PolicyVersion != nil)
		}
		if !found {
			t.Errorf("Event 应记 basis（策略版本与规则 id）: %+v", tl.Steps)
		}
	})

	step(t, "ACT-RULE-05 off 时跳过", func(t *testing.T) {
		f.webOK("PATCH", modePath(tagKey), `{"mode":"off"}`, 200)
		id := f.openItem("acc-rule")
		time.Sleep(3 * time.Second)
		if f.show(id).has("acc-hit") || strings.Contains(f.why(id), "rule.observed") {
			t.Error("off 时不得执行也不得有观察记录")
		}
	})

	step(t, "ACT-RULE-06 规则动作不再触发规则", func(t *testing.T) {
		writeFile(t, filepath.Join(f.packDir("acc-loop"), "pack.yaml"), accLoopPack)
		waitFor(t, "两条规则应已热加载", 30*time.Second, func() bool { r := f.rules(); _, a := r["loop-a"]; _, b := r["loop-b"]; return a && b })
		for _, k := range []string{"acc-loop/event_rules/loop-a", "acc-loop/event_rules/loop-b"} {
			f.webOK("PATCH", modePath(k), `{"mode":"auto"}`, 200)
		}
		id := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "loop", "--tags", "acc-loop", "--reason", "rule e2e", "--evidence", "rule e2e").Stdout).ID
		waitFor(t, "第一层动作 set_phase=waiting 应发生", 30*time.Second, func() bool { return f.show(id).Phase == "waiting" })
		f.TCAdvance(60) // 触发后等 1 分钟
		time.Sleep(3 * time.Second)
		it := f.show(id)
		if it.Phase != "waiting" || it.has("acc-b") {
			t.Errorf("只应发生第一层动作，规则 A 的动作 Event 不应再命中规则 B: %+v", it)
		}
		n := 0
		for _, e := range f.events(id, "") {
			if e.Actor.Via == "rule" {
				n++
			}
		}
		if n == 0 || n > 3 {
			t.Errorf("规则产生的 Event 应有限（1..3），实际 %d", n)
		}
	})

	step(t, "ACT-RULE-07 动作类规则内容变化后回到 observe", func(t *testing.T) {
		f.webOK("PATCH", modePath(tagKey), `{"mode":"auto"}`, 200)
		before := f.healthDeliveries()
		writeFile(t, filepath.Join(f.packDir("acc-rule"), "pack.yaml"), strings.Replace(accTagPack, "tag: acc-hit", "tag: acc-hit2", 1))
		waitFor(t, "热加载后内容变化的规则应回到 observe", 30*time.Second, func() bool { return f.rules()["acc-tag"].Mode == "observe" })
		waitFor(t, "应产出一条健康类投递", 30*time.Second, func() bool { return f.healthDeliveries() > before })
		id := f.openItem("acc-rule")
		waitFor(t, "再次触发只记录", 30*time.Second, func() bool { return strings.Contains(f.why(id), "rule.observed") })
		if sh := f.show(id); sh.has("acc-hit") || sh.has("acc-hit2") {
			t.Errorf("回到 observe 后只应记录: %+v", sh)
		}
	})

	step(t, "ACT-RULE-10 类别由加载器推定，能力包不能自报", func(t *testing.T) {
		writeFile(t, filepath.Join(f.packDir("acc-cat"), "pack.yaml"), accCatPack(true))
		waitFor(t, "带 mode: auto 的包整包被拒绝", 30*time.Second, func() bool {
			h := f.healthText()
			return strings.Contains(h, "acc-cat") && strings.Contains(h, "cat-follow")
		})
		r := f.rules()
		for _, id := range []string{"cat-timer", "cat-stale", "cat-follow"} {
			if _, ok := r[id]; ok {
				t.Fatalf("整包被拒绝时 %s 不应出现", id)
			}
		}
		writeFile(t, filepath.Join(f.packDir("acc-cat"), "pack.yaml"), accCatPack(false))
		waitFor(t, "删去 mode: auto 后整包加载", 30*time.Second, func() bool { _, ok := f.rules()["cat-timer"]; return ok })
		r = f.rules()
		if r["cat-timer"].Mode != "observe" {
			t.Errorf("只建提醒 Timer 的事件规则仍属动作类，应 observe: %+v", r["cat-timer"])
		}
		if r["cat-stale"].Mode != "auto" || r["cat-follow"].Mode != "auto" {
			t.Errorf("stale、follow_up 规则应为 auto: %+v %+v", r["cat-stale"], r["cat-follow"])
		}
	})

	step(t, "ACT-RULE-12 提醒类规则可在 Web 关掉，内容变化后保持本人的选择", func(t *testing.T) {
		const staleKey = "follow-up/clock_rules/stale-check"
		f.webOK("PATCH", modePath(staleKey), `{"mode":"off"}`, 200)
		if evs := f.events("rule:"+staleKey, "rule.mode_changed"); len(evs) != 1 || evs[0].Actor.Type != "human" {
			t.Fatalf("切 off 应有一条本人的 rule.mode_changed: %+v", evs)
		}
		before := f.healthDeliveries()
		f.shortPack(func(s string) string {
			return strings.Replace(strings.Replace(s, "schedule: 1m", "schedule: 72h", 1), "schedule: 24h", `schedule: "0 9 * * *"`, 1)
		})
		waitFor(t, "stale-check 的新 schedule 生效", 30*time.Second, func() bool { return strings.Contains(f.webOK("GET", "/packs", "", 200), `"0 9 * * *"`) })
		f.shortPack(func(s string) string { return strings.Replace(s, "schedule: 1m", "schedule: 70h", 1) })
		waitFor(t, "follow-up-interval 的新 schedule 生效", 30*time.Second, func() bool { return f.schedule() == "70h" })
		r := f.rules()
		if r["stale-check"].Mode != "off" || r["follow-up-interval"].Mode != "auto" {
			t.Errorf("内容变化后 stale-check 仍应为 off、follow-up-interval 仍为 auto: %+v %+v", r["stale-check"], r["follow-up-interval"])
		}
		if f.healthDeliveries() != before || strings.Contains(f.healthText(), "back to observe") && strings.Contains(f.healthText(), "follow-up") {
			t.Error("两次热加载都不应产生\"规则回退 observe\"的健康类投递")
		}
	})

	t.Run("全新安装", func(t *testing.T) { freshInstall(t) })
	t.Run("升级", func(t *testing.T) { upgrade(t) })
}

// freshInstall 对应 ACT-RULE-08、09、11：全新空目录 apa init + apa serve，不做任何设置、不配模型端点。
func freshInstall(t *testing.T) {
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	f := &fx{e, t}

	step(t, "ACT-RULE-08 全新安装时内置提醒类规则开箱为 auto", func(t *testing.T) {
		r := f.rules()
		for _, id := range []string{"follow-up-interval", "stale-check"} {
			if r[id].Mode != "auto" {
				t.Errorf("%s 应开箱为 auto: %+v", id, r[id])
			}
		}
		if n := len(f.events("", "rule.mode_changed")); n != 0 {
			t.Errorf("审计页不应有任何 rule.mode_changed（不需要本人切换），实际 %d", n)
		}
	})

	step(t, "ACT-RULE-09 全新安装时动作类规则一律 observe", func(t *testing.T) {
		n := 0
		for id, s := range f.rules() {
			if id == "follow-up-interval" || id == "stale-check" {
				continue
			}
			n++
			if s.Mode != "observe" && s.Mode != "off" {
				t.Errorf("动作类规则 %s 初始为 %s，应为 observe（或包里显式写的 off）", s.Key, s.Mode)
			}
		}
		if n == 0 {
			t.Error("没有可检查的动作类规则")
		}
	})

	step(t, "ACT-RULE-11 截止提醒开箱生效", func(t *testing.T) {
		id := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--phase", "open", "--title", "RULE-11 截止提醒", "--evidence", "owner: add").Stdout).ID
		f.webOK("PATCH", "/items/"+id, `{"deadline":"`+time.Now().Add(time.Minute).UTC().Format(time.RFC3339)+`"}`, 200)
		f.TCAdvance(120)
		waitFor(t, "截止已过", 30*time.Second, func() bool { return len(f.events(id, "item.deadline_passed")) == 1 })
		// 默认策略：提醒走宿主注入，低优先级提醒合并窗口 30 分钟；没有宿主在线，窗口过后落到 Web 未读。
		f.TCAdvance(1800)
		waitFor(t, "应有一条指向该事项的到期提醒", 30*time.Second, func() bool {
			for _, d := range f.unread() {
				if d.Item == id && d.Class == "reminder" {
					return true
				}
			}
			return false
		})
		evs := f.events(id, "item.deadline_passed")
		if len(evs) == 0 || evs[0].Actor.Type != "system" {
			t.Errorf("到期应由 system 触发: %+v", evs)
		}
		if f.ModelStub != nil {
			t.Error("该实例未配端点")
		}
		if h := f.healthText(); !strings.Contains(h, `"endpoint":""`) {
			t.Errorf("该实例应未配置模型端点: %s", h)
		}
	})
}

// ---- 升级 ----

func maxSeq(t *testing.T, i *inst) (m int64) {
	t.Helper()
	for after := int64(0); ; {
		code, body := i.web("GET", "/v1/audit/events?limit=500&after="+strconv.FormatInt(after, 10), "")
		if code != 200 {
			t.Fatalf("审计页: %d %s", code, body)
		}
		page := decode[struct {
			Events []auditEvent `json:"events"`
		}](t, body).Events
		for _, e := range page {
			m = max(m, e.Seq)
		}
		if len(page) < 500 {
			return m
		}
		after = m
	}
}

type ruleSnap struct {
	Mode string `json:"mode"`
	Hash string `json:"hash"`
}

func ruleSnapshot(t *testing.T, body string) map[string]ruleSnap {
	t.Helper()
	out := map[string]ruleSnap{}
	for _, r := range decode[struct {
		Rules []struct {
			Key  string `json:"key"`
			Mode string `json:"mode"`
			Hash string `json:"hash"`
		} `json:"rules"`
	}](t, body).Rules {
		out[r.Key] = ruleSnap{r.Mode, r.Hash}
	}
	return out
}

// modeChanges 返回 after 之后的 rule.mode_changed（按规则键）。
func modeChanges(t *testing.T, i *inst, token string, after int64) map[string]auditEvent {
	t.Helper()
	code, out := i.apa(token, "events", "--cursor", strconv.FormatInt(after, 10))
	if code != 0 {
		t.Fatalf("apa events: %d %s", code, out)
	}
	m := map[string]auditEvent{}
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		e := decode[auditEvent](t, ln)
		if e.Type == "rule.mode_changed" {
			m[strings.TrimPrefix(e.Object, "rule:")] = e
		}
	}
	return m
}

const followKey = "follow-up/clock_rules/follow-up-interval"

// upgrade 对应 ACT-RULE-13..16：三份 v0.1 结构夹具副本，先用 v0.1 二进制留下升级前快照，再用待测版本启动。
func upgrade(t *testing.T) {
	e := baseline.New(t)
	v01 := e.V01Bin()
	var fxA, fxB, fxC *baseline.Fixture
	type before struct {
		rules map[string]ruleSnap
		seq   int64
	}
	var snap [3]before

	step(t, "ACT-RULE-13 准备三份 v0.1 结构夹具副本与升级前的规则模式快照", func(t *testing.T) {
		fxA, fxB, fxC = e.LedgerFixture(2), e.LedgerFixture(2), e.LedgerFixture(2)
		// FX_A、FX_B 要的是「未动过的内置规则」：夹具自带的用户包 follow-up（v0.1 写法、显式 mode: observe）会被 v0.2.0 整包拒绝，
		// 本人写的 mode 看不到，升级不替他切换（GT-345、§16.5），所以先去掉它，让内置 follow-up 包生效。
		for _, fx := range []*baseline.Fixture{fxA, fxB} {
			if err := os.RemoveAll(filepath.Join(fx.Home, "packs", "follow-up")); err != nil {
				t.Fatal(err)
			}
		}
		// FX_C：覆盖内置 follow-up 包的用户包，按 v0.2.0 写法（不写 do[]）显式写 mode: observe。
		dst := filepath.Join(fxC.Home, "packs", "follow-up")
		_ = os.RemoveAll(dst)
		if err := os.CopyFS(dst, os.DirFS("../../../packs/follow-up")); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dst, "pack.yaml")
		b, _ := os.ReadFile(p)
		writeFile(t, p, strings.Replace(string(b), "    schedule: 72h\n", "    schedule: 72h\n    mode: observe\n", 1))
		for k, fx := range []*baseline.Fixture{fxA, fxB, fxC} {
			v := startInst(t, v01, fx.Home, "")
			v.login(fx.M.WebSecret)
			if k == 1 { // FX_B：在 v0.1 Web 设置页把内置 follow-up-interval 切到 off
				if code, body := v.web("PATCH", "/v1"+modePath(followKey), `{"mode":"off"}`); code != 200 {
					t.Fatalf("v0.1 切 off: %d %s", code, body)
				}
			}
			code, body := v.api("GET", "/v1/rules", "", fx.M.Tokens["observer"])
			if code != 200 {
				t.Fatalf("v0.1 GET /v1/rules: %d %s", code, body)
			}
			prior := decode[struct {
				Events []auditEvent `json:"events"`
			}](t, func() string {
				_, b := v.web("GET", "/v1/audit/events?limit=500&type=rule.mode_changed&object="+url.QueryEscape("rule:"+followKey), "")
				return b
			}()).Events
			if want := []int{0, 1, 0}[k]; len(prior) != want { // FX_A、FX_C 从未有，FX_B 恰一条本人的
				t.Fatalf("夹具 %d 里 follow-up-interval 的 rule.mode_changed 数 = %d", k, len(prior))
			}
			if k == 1 && prior[0].Actor.Type != "human" {
				t.Fatalf("FX_B 的切换应是本人: %+v", prior[0])
			}
			snap[k] = before{ruleSnapshot(t, body), maxSeq(t, v)}
			v.stop()
		}
		if snap[0].rules[followKey].Mode != "observe" || snap[1].rules[followKey].Mode != "off" {
			t.Fatalf("FX_A 应为 observe、FX_B 应为 off: %+v %+v", snap[0].rules[followKey], snap[1].rules[followKey])
		}
	})

	// 升级后的 serve 在 fx 上运行；tok 是夹具的 observer token。
	up := func(fx *baseline.Fixture) (*inst, string) {
		v := startInst(t, e.Bin["apa"], fx.Home, "")
		v.login(fx.M.WebSecret)
		return v, fx.M.Tokens["observer"]
	}
	rulesNow := func(v *inst, tok string) map[string]ruleSnap {
		code, body := v.api("GET", "/v1/rules", "", tok)
		if code != 200 {
			t.Fatalf("GET /v1/rules: %d %s", code, body)
		}
		return ruleSnapshot(t, body)
	}

	step(t, "ACT-RULE-14 升级时未动过的内置提醒类规则切到 auto 并留痕", func(t *testing.T) {
		v, tok := up(fxA)
		after := rulesNow(v, tok)
		if after[followKey].Mode != "auto" {
			t.Fatalf("follow-up-interval 应为 auto: %+v", after[followKey])
		}
		ch := modeChanges(t, v, tok, snap[0].seq)
		if c, ok := ch[followKey]; !ok || c.Actor.Type != "system" {
			t.Fatalf("升级前最大序号之后应有一条该规则的 rule.mode_changed，操作者 system: %+v", ch)
		}
		// 动作类规则：包内容没变的，mode 与升级前完全相同；包内容随版本变了的，按 §14.8 回到 observe 并各有一条健康说明。
		actionRules := 0
		for k, b := range snap[0].rules {
			if k == followKey || strings.Contains(k, "stale-check") {
				continue
			}
			actionRules++
			a := after[k]
			if a.Hash == b.Hash && a.Mode != b.Mode {
				t.Errorf("动作类规则 %s 内容未变，mode 应保持 %s，实际 %s", k, b.Mode, a.Mode)
			}
			if a.Hash != b.Hash && a.Mode != "observe" && a.Mode != "off" {
				t.Errorf("动作类规则 %s 内容变化后不得为 auto: %s", k, a.Mode)
			}
		}
		if actionRules == 0 {
			t.Error("夹具里没有动作类规则可比对")
		}
		// 所有被切到 auto 的提醒类规则合并为一条健康类说明（不是每条规则一条）。
		code, body := v.web("GET", "/v1/deliveries/unread", "")
		if code != 200 {
			t.Fatalf("unread: %d %s", code, body)
		}
		merged := 0
		for _, d := range decode[struct {
			Deliveries []delivery `json:"deliveries"`
		}](t, body).Deliveries {
			if d.Class == "health" && strings.Contains(d.Content.Summary, "切到 auto") {
				merged++
				if !strings.Contains(d.Content.Summary, followKey) {
					t.Errorf("说明应列出被切换的规则: %s", d.Content.Summary)
				}
			}
		}
		if merged != 1 {
			t.Errorf("被切换的规则应合并为一条健康类说明，实际 %d 条", merged)
		}
		// 第二次启动不再新增 rule.mode_changed。
		first := len(modeChanges(t, v, tok, snap[0].seq))
		v.stop()
		v2, tok := up(fxA)
		if second := len(modeChanges(t, v2, tok, snap[0].seq)); second != first {
			t.Errorf("第二次启动不应再新增 rule.mode_changed: %d → %d", first, second)
		}
		if rulesNow(v2, tok)[followKey].Mode != "auto" {
			t.Error("重启后 follow-up-interval 仍应为 auto")
		}
	})

	step(t, "ACT-RULE-15 升级时本人切换过的规则保持原样", func(t *testing.T) {
		v, tok := up(fxB)
		if m := rulesNow(v, tok)[followKey].Mode; m != "off" {
			t.Errorf("follow-up-interval 仍应为 off，实际 %s", m)
		}
		if c, ok := modeChanges(t, v, tok, snap[1].seq)[followKey]; ok {
			t.Errorf("升级后不应有它的 rule.mode_changed: %+v", c)
		}
	})

	step(t, "ACT-RULE-16 升级时用户覆盖包里显式写的模式按包里的写法", func(t *testing.T) {
		v, tok := up(fxC)
		if m := rulesNow(v, tok)[followKey].Mode; m != "observe" {
			t.Errorf("follow-up-interval 来自用户覆盖包，应为 observe，实际 %s", m)
		}
		if c, ok := modeChanges(t, v, tok, snap[2].seq)[followKey]; ok {
			t.Errorf("不应有把它切到 auto 的 rule.mode_changed: %+v", c)
		}
	})
}
