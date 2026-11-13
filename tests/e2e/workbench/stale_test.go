//go:build e2e

package workbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	staleRuleKey = "follow-up/clock_rules/stale-check"
	// 提醒只走 Web 未读，且不合并窗口：e2e 里按"一次检查一条"数提醒。
	webOnlyPolicy = `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false}}`
)

// copyFollowUpPack 把内置 follow-up 包复制到 $APA_HOME/packs/follow-up，并按 edit 改写 pack.yaml（须真的改动了内容）。
func (k *kit) copyFollowUpPack(edit func(string) string) {
	k.t.Helper()
	dst := filepath.Join(k.Home, "packs", "follow-up")
	k.must(os.CopyFS(dst, os.DirFS("../../../packs/follow-up")))
	p := filepath.Join(dst, "pack.yaml")
	b, err := os.ReadFile(p)
	k.must(err)
	out := edit(string(b))
	if out == string(b) {
		k.t.Fatal("follow-up 包的覆盖没有改动任何内容，内置包的写法变了")
	}
	k.must(os.WriteFile(p, []byte(out), 0o644))
}

// rule 取 GET /rules 里某条规则（key 形如 pack/clock_rules/id）。
func (k *kit) rule(key string) map[string]any {
	k.t.Helper()
	var out struct {
		Rules []map[string]any `json:"rules"`
	}
	jsonOf(k.t, k.webOK("GET", "/rules", "", 200), &out)
	for _, r := range out.Rules {
		if r["key"] == key {
			return r
		}
	}
	k.t.Fatalf("GET /rules 里没有 %s: %v", key, out.Rules)
	return nil
}

// remindersMentioning 取 Web 未读里正文提到全部给定事项标题的投递条数。
func (k *kit) remindersMentioning(titles ...string) int {
	k.t.Helper()
	var out struct {
		Deliveries []json.RawMessage `json:"deliveries"`
	}
	jsonOf(k.t, k.unreadRaw(), &out)
	n := 0
	for _, d := range out.Deliveries {
		all := true
		for _, title := range titles {
			all = all && strings.Contains(string(d), "「"+title+"」")
		}
		if all {
			n++
		}
	}
	return n
}

// advance 把测试时钟分 step 秒一步地推进 total 秒，每步给内核一点时间处理到期的检查。
func (k *kit) advance(total, step int64) {
	k.t.Helper()
	for done := int64(0); done < total; done += step {
		k.TCAdvance(step)
		time.Sleep(700 * time.Millisecond)
	}
}

// TestFlowStale 走完 flow-stale（ACT-STL-01..06）：久未推进的任务与承诺合并问一次去留，本人批量选择，系统不自动改状态。
// 时间用测试时钟推进（apa_testclock 构建的偏移量文件），不真等分钟。
func TestFlowStale(t *testing.T) {
	k := newKit(t)
	var st [9]string // ST-1..ST-8，下标从 1 起
	day2 := day(2)

	k.step("ACT-STL-01 缩短久未推进的阈值与检查周期并准备事项", func(t *testing.T) {
		k.copyFollowUpPack(func(s string) string {
			return strings.Replace(s, "schedule: 24h\n    stale_after: 14d", "schedule: \"*/2 * * * *\"\n    stale_after: 3m", 1)
		})
		k.restartKernel()
		k.webOK("PUT", "/settings/policy", webOnlyPolicy, 200)
		r := k.rule(staleRuleKey)
		eq(t, "stale-check 模式", r["mode"], "auto")
		if p := k.webOK("GET", "/packs", "", 200); !strings.Contains(p, `{"id":"stale-check","type":"stale","schedule":"*/2 * * * *"}`) {
			i := strings.Index(p, `"name":"follow-up"`)
			t.Errorf("follow-up 包没有按 3m 与 */2 覆盖生效: %.900s", p[i:]) // /packs 不回显 stale_after；阈值由 ACT-STL-02 的行为证明
		}
		st[1] = k.webItem(map[string]any{"kind": "task", "title": "ST-1"})
		st[2] = k.webItem(map[string]any{"kind": "task", "title": "ST-2"})
		st[3] = k.webItem(map[string]any{"kind": "commitment", "title": "ST-3"})
		st[4] = k.webItem(map[string]any{"kind": "task", "title": "ST-4", "planned_for": day2})
		st[5] = k.webItem(map[string]any{"kind": "task", "title": "ST-5"})
		k.apa(k.DEL, 0, "item", "transition", st[5], "--to", "waiting", "--waiting-on", "external:等对方回复", "--reason", "等回复", "--evidence", "等对方回复")
		st[6] = k.webItem(map[string]any{"kind": "decision", "title": "ST-6"})
		for i := 1; i <= 6; i++ {
			if st[i] == "" {
				t.Fatalf("ST-%d 没建好", i)
			}
		}
		eq(t, "ST-5 阶段", k.phase(st[5]), "waiting")
		// 之后 4 分钟内不碰 ST-1..ST-6，只在第 2 分钟给 ST-2 写一条进展。
		k.advance(120, 30)
		k.webOK("POST", "/items/"+st[2]+"/progress", `{"text":"有进展"}`, 201)
		k.advance(120, 30)
	})

	var cls0, model0 int
	k.step("ACT-STL-02 超过阈值的任务与承诺列入\"今天\"的久未推进一组", func(t *testing.T) {
		cls0, model0 = len(k.Cls.Batches()), len(k.ModelStub.Requests())
		cli, web := k.today(), k.webToday()
		for name, td := range map[string]todayList{"CLI": cli, "Web": web} {
			g := td.ids("stale")
			if !has(g, st[1]) || !has(g, st[3]) {
				t.Errorf("%s: stale 组缺 ST-1/ST-3: %v", name, g)
			}
			for i, why := range map[int]string{2: "有近期进展", 4: "有未来计划日期", 5: "不是 open", 6: "不是任务或承诺"} {
				if has(g, st[i]) {
					t.Errorf("%s: stale 组误列 ST-%d（%s）", name, i, why)
				}
			}
		}
	})

	k.step("ACT-STL-03 检查到期时合并成一条提醒，不调模型", func(t *testing.T) {
		if !k.WaitFor(45*time.Second, func() bool {
			k.advance(30, 30)
			return k.remindersMentioning("ST-1", "ST-3") > 0
		}) {
			t.Fatalf("没有久未推进提醒: %s", k.unreadRaw())
		}
		eq(t, "同时列出 ST-1 与 ST-3 的提醒条数", k.remindersMentioning("ST-1", "ST-3"), 1)
		eq(t, "只含 ST-1 的提醒条数（不得每个事项各一条）", k.remindersMentioning("ST-1"), 1)
		eq(t, "只含 ST-3 的提醒条数", k.remindersMentioning("ST-3"), 1)
		if !strings.Contains(k.unreadRaw(), "/today#stale") {
			t.Errorf("提醒没有链接到\"今天\"的久未推进一组: %s", k.unreadRaw())
		}
		// 同一批再检查一次不重复：给 ST-2 再写一条进展让它仍不算久未推进，批保持 {ST-1, ST-3}。
		k.webOK("POST", "/items/"+st[2]+"/progress", `{"text":"还在做"}`, 201)
		k.advance(150, 30)
		eq(t, "第二次检查后的提醒条数", k.remindersMentioning("ST-1", "ST-3"), 1)
		eq(t, "CLS_LOG 行数", len(k.Cls.Batches()), cls0)
		eq(t, "MODEL_LOG 行数", len(k.ModelStub.Requests()), model0)
	})

	k.step("ACT-STL-04 系统不自动改状态", func(t *testing.T) {
		k.advance(300, 60)
		eq(t, "ST-1 阶段", k.phase(st[1]), "open")
		eq(t, "ST-3 阶段", k.phase(st[3]), "open")
		for _, id := range []string{st[1], st[3]} {
			for _, e := range k.events(id, "") {
				if e.Actor.Type == "system" && strings.HasPrefix(e.Type, "item.") && e.Type != "item.created" {
					t.Errorf("事项 %s 上有系统发起的变更: %+v", id, e)
				}
			}
		}
	})

	k.step("ACT-STL-05 本人批量选继续、改期、放弃", func(t *testing.T) {
		st[7] = k.webItem(map[string]any{"kind": "task", "title": "ST-7"})
		if !k.WaitFor(60*time.Second, func() bool {
			k.advance(60, 30)
			return has(k.webToday().ids("stale"), st[7])
		}) {
			t.Fatal("ST-7 没有进入久未推进组")
		}
		// "今天"页的批量提交逐条调既有接口：继续=记一条进展，改期=设计划日期，放弃=转 dropped；每条一个 Event。
		k.webOK("POST", "/items/"+st[1]+"/progress", `{"text":"继续"}`, 201)
		k.webOK("PATCH", "/items/"+st[3], `{"planned_for":"`+day2+`"}`, 200)
		k.webOK("POST", "/items/"+st[7]+"/transition", `{"to":"dropped","reason":"久未推进，本人选择放弃"}`, 200)
		eq(t, "ST-1 阶段", k.phase(st[1]), "open")
		eq(t, "ST-3 planned_for", k.planned(st[3]), day2)
		eq(t, "ST-7 阶段", k.phase(st[7]), "dropped")
		stale := k.webToday().ids("stale")
		if has(stale, st[3]) || has(stale, st[7]) || has(stale, st[1]) {
			t.Errorf("处理后久未推进组还有: %v", stale)
		}
		byOwner(t, "ST-1 继续", k.lastEvent(st[1], "item.progress_added"))
		byOwner(t, "ST-3 改期", k.lastEvent(st[3], "item.updated"))
		byOwner(t, "ST-7 放弃", k.lastEvent(st[7], "item.phase_changed"))
	})

	k.step("ACT-STL-06 默认阈值为 14 天", func(t *testing.T) {
		k.must(os.RemoveAll(filepath.Join(k.Home, "packs", "follow-up")))
		k.restartKernel()
		r := k.rule(staleRuleKey)
		eq(t, "模式", r["mode"], "auto")
		if p := k.webOK("GET", "/packs", "", 200); !strings.Contains(p, `{"id":"stale-check","type":"stale","schedule":"24h"}`) {
			t.Errorf("内置 stale-check 不是 type stale、每天检查")
		}
		st[8] = k.webItem(map[string]any{"kind": "task", "title": "ST-8"})
		k.advance(600, 60)
		if has(k.today().ids("stale"), st[8]) {
			t.Error("新事项 ST-8 被误列入久未推进组")
		}
		// 14 天的边界：13 天后仍不算，再过 2 天才算。
		k.TCAdvance(13 * 24 * 3600)
		k.WebLogin()
		if has(k.webToday().ids("stale"), st[8]) {
			t.Error("ST-8 在 13 天后就被列入久未推进组，默认阈值不是 14 天")
		}
		k.TCAdvance(2 * 24 * 3600)
		k.WebLogin()
		if !has(k.webToday().ids("stale"), st[8]) {
			t.Error("ST-8 在 15 天后仍不在久未推进组")
		}
	})
}
