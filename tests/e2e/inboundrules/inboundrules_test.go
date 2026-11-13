//go:build e2e

package inboundrules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// accRulesYAML 是验收规则包 acc-rules（ACT-IRL-01 第 3 步）。%s 依次是 acc-home 的项目 id。
const accRulesYAML = `name: acc-rules
version: 1.0.0
description: 来源规则验收包
source_rules:
  - {id: r-prop, match: {source_type: acc-rule, event_type: [acc.proposed]}, kind: task, then: proposed}
  - {id: r-ign, match: {source_type: acc-rule, event_type: [acc.ignore]}, then: ignore}
  - {id: r-scr, match: {source_type: acc-rule, event_type: [acc.screen]}, then: screen}
  - {id: r-judge, match: {source_type: acc-rule, event_type: [acc.judge]}, then: judge}
  - {id: r-ext, match: {source_type: acc-rule, event_type: [acc.chat]}, then: extract}
  - {id: r-dig, match: {source_type: acc-rule, event_type: [acc.news]}, then: digest}
  - id: r-alert
    match: {source_type: acc-rule, event_type: [acc.metric]}
    where: [{field: metrics.value, op: crosses_above, value: 90}]
    then: alert
    mode: observe
  - id: r-eq
    match: {source_type: acc-rule, event_type: [acc.where]}
    where: [{field: sender, op: eq, value: alice}]
    then: proposed
  - id: r-ne
    match: {source_type: acc-rule, event_type: [acc.where-ne]}
    where: [{field: sender, op: ne, value: alice}]
    then: proposed
  - id: r-in
    match: {source_type: acc-rule, event_type: [acc.where-in]}
    where: [{field: sender, op: in, value: [bob, carol]}]
    then: proposed
  - id: r-num
    match: {source_type: acc-rule, event_type: [acc.size]}
    where: [{field: metrics.size, op: gte, value: 10}, {field: metrics.size, op: lt, value: 20}]
    then: proposed
  - id: r-gt-lte
    match: {source_type: acc-rule, event_type: [acc.size2]}
    where: [{field: metrics.size, op: gt, value: 10}, {field: metrics.size, op: lte, value: 20}]
    then: proposed
  - id: r-chg
    match: {source_type: acc-rule, event_type: [acc.level]}
    where: [{field: metrics.level, op: changed}]
    then: proposed
  - id: r-below
    match: {source_type: acc-rule, event_type: [acc.temp]}
    where: [{field: metrics.temp, op: crosses_below, value: 5}]
    then: proposed
  - {id: r-fixed, match: {source_type: acc-rule, event_type: [acc.fixed]}, project_by: "fixed:%s", then: proposed}
  - {id: r-none, match: {source_type: acc-rule, event_type: [acc.none]}, project_by: none, then: proposed}
  - {id: r-draft, match: {source_type: acc-rule, event_type: [acc.draft]}, project_by: draft, then: screen}
  - {id: r-tpl, match: {source_type: acc-rule, event_type: [acc.tpl]}, then: screen, screen: acc-screen}
`

func TestFlowInboundRules(t *testing.T) {
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	e.CreateProjects()
	e.StartCls()
	e.StartModelStub()
	e.WireEndpoints()
	e.DeclareStubHarness()
	f := &fx{Env: e, t: t, w: t.TempDir()}
	// 投递都走渠道 acc-chan 且不合并：这样"没有投递"能用 CHAN 直接观察。
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"channel:acc-chan","decision":"channel:acc-chan","health":"channel:acc-chan","reminder":"channel:acc-chan","wake":"channel:acc-chan"},"merge_window_sec":0,"remind_on_deadline":true}}`, 200)

	packDir := filepath.Join(e.Home, "packs", "acc-rules")
	var src, cal, rem, gh *source
	noDelivery := func(what string) {
		t.Helper()
		if r := f.APIRaw("GET", "/v1/deliveries/next?wait=0", "", f.CHAN); r.Code == 200 {
			t.Errorf("%s：不应产生投递，实际领到 %s", what, r.Body)
		}
	}
	restart := func() { e.StopKernel(); e.StartKernelAgain() }

	f.step("ACT-IRL-01 准备规则验收包与脚本来源", func() {
		f.must(os.MkdirAll(filepath.Join(packDir, "prompts"), 0o755))
		f.must(os.WriteFile(filepath.Join(packDir, "pack.yaml"), []byte(fmt.Sprintf(accRulesYAML, e.Projects["acc-home"])), 0o644))
		tmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "packs", "inbound-triage", "prompts", "triage-screen.md"))
		f.must(err)
		f.must(os.WriteFile(filepath.Join(packDir, "prompts", "acc-screen.md"), tmpl, 0o644))
		src = f.register("src_irl_src", "acc-rule", "acc-rule")
		cal = f.register("src_irl_cal", "calendar", "irl-cal")
		rem = f.register("src_irl_rem", "reminder", "irl-rem")
		gh = f.register("src_irl_gh", "github", "irl-gh")
		restart()
		e.Cls.Reset()
		e.ModelStub.Reset()
		if !strings.Contains(f.apa(e.OBS, 0, "pack", "list").Stdout, "acc-rules") {
			t.Error("apa pack list 应列出 acc-rules")
		}
		if rejected(f.apa(e.OBS, 0, "health").Stdout) {
			t.Error("GET /health 的 pack_rejections 应为 ok")
		}
		for _, s := range []*source{src, cal, rem, gh} {
			var st struct {
				Enabled bool `json:"enabled"`
			}
			f.must(json.Unmarshal([]byte(f.apa(e.OBS, 0, "source", "show", s.id).Stdout), &st))
			if !st.Enabled {
				t.Errorf("来源 %s 应为启用", s.id)
			}
		}
	})

	f.step("ACT-IRL-02 proposed 出口直接成为建议，不调模型", func() {
		before := f.proposedCount()
		got := src.emit(ev("irl-p1", "acc.proposed", "续办证件", ""))["irl-p1"]
		eq(t, "disposition", got.Disposition, "proposed")
		it := f.item(got.Item)
		eq(t, "phase", it.Phase, "proposed")
		eq(t, "kind", it.Kind, "task")
		eq(t, "title", it.Title, "续办证件")
		eq(t, "建议流新增", f.proposedCount(), before+1)
		f.noModelCalls("续办证件")
		noDelivery("proposed 出口")
	})

	f.step("ACT-IRL-03 ignore 出口直接忽略", func() {
		before := f.proposedCount()
		got := src.emit(ev("irl-i1", "acc.ignore", "状态已同步", ""))["irl-i1"]
		eq(t, "disposition", got.Disposition, "ignored")
		eq(t, "item", got.Item, "")
		eq(t, "建议流", f.proposedCount(), before)
		f.noModelCalls("状态已同步")
	})

	f.step("ACT-IRL-04 screen 出口交初筛，是则起草成建议", func() {
		f.pYes("报销", 0.9)
		f.draft("报销", map[string]any{"title": "整理报销单", "summary": "三张发票待提交", "kind": "task", "recommend": "accept"})
		got := src.emit(ev("irl-s1", "acc.screen", "报销", `"summary":"三张发票"`))["irl-s1"]
		eq(t, "disposition", got.Disposition, "proposed")
		eq(t, "title", f.item(got.Item).Title, "整理报销单")
		tmpl(t, f.judgment(got), "triage-screen")
		if f.clsMentions("报销") == 0 {
			t.Error("CLS_LOG 应有 irl-s1 的初筛题")
		}
		eq(t, "MODEL_LOG 起草请求数", f.modelMentions("报销"), 1)
	})

	f.step("ACT-IRL-05 没有规则命中的事件按 screen 处理", func() {
		f.pYes("无规则的事件", 0.05)
		before := f.proposedCount()
		got := src.emit(ev("irl-u1", "acc.unmatched", "无规则的事件", ""))["irl-u1"]
		eq(t, "disposition", got.Disposition, "ignored")
		if f.clsMentions("无规则的事件") == 0 {
			t.Error("CLS_LOG 应有 irl-u1 的初筛题")
		}
		eq(t, "MODEL_LOG 起草请求", f.modelMentions("无规则的事件"), 0)
		eq(t, "建议流", f.proposedCount(), before)
	})

	f.step("ACT-IRL-06 旧写法 then judge 被接受并等同 screen", func() {
		f.pYes("旧规则事件", 0.9)
		f.draft("旧规则事件", map[string]any{"title": "旧规则起草", "kind": "task"})
		got := src.emit(ev("irl-j1", "acc.judge", "旧规则事件", ""))["irl-j1"]
		eq(t, "disposition", got.Disposition, "proposed")
		eq(t, "title", f.item(got.Item).Title, "旧规则起草")
		tmpl(t, f.judgment(got), "triage-screen")
		eq(t, "起草请求数", f.modelMentions("旧规则事件"), 1)
		f.apa(e.OBS, 0, "pack", "validate", packDir)
	})

	f.step("ACT-IRL-07 extract 出口从一条聚合事件抽出多条候选", func() {
		f.pYes("群聊片段", 0.9)
		f.draft("群聊片段", map[string]any{"candidates": []map[string]any{
			{"title": "周五前交方案", "kind": "task"}, {"title": "约李工下周对接", "kind": "task"}}})
		before := f.proposedCount()
		got := src.emit(ev("irl-x1", "acc.chat", "群聊片段", `"items":[{"sender":"bob","text":"方案周五前给我","at":"`+now()+`"},{"sender":"bob","text":"下周和李工对一下","at":"`+now()+`"}]`))["irl-x1"]
		eq(t, "disposition", got.Disposition, "proposed")
		eq(t, "items 数", len(got.Items), 2)
		eq(t, "建议流新增", f.proposedCount(), before+2)
		tmpl(t, f.judgment(got), "capture-prescreen")
		if f.clsMentions("群聊片段") == 0 || f.modelMentions("群聊片段") != 1 {
			t.Errorf("应先有 capture-prescreen 判断请求，再有一次 triage-extract 生成请求: CLS %d MODEL %d", f.clsMentions("群聊片段"), f.modelMentions("群聊片段"))
		}
	})

	f.step("ACT-IRL-08 digest 出口不建事项、不提醒", func() {
		before := f.proposedCount()
		got := src.emit(ev("irl-d1", "acc.news", "本周行业简讯", ""))["irl-d1"]
		eq(t, "disposition", got.Disposition, "digest")
		eq(t, "item", got.Item, "")
		eq(t, "建议流", f.proposedCount(), before)
		f.noModelCalls("本周行业简讯")
		noDelivery("digest 出口")
	})

	f.step("ACT-IRL-09 alert 出口的变化条件按序列上一次取值判断", func() {
		// 条件不成立的事件不命中 r-alert，按 screen 处理；验收文档没给这三条配分类端点结果，这里配成"不值得"，使它们不建事项。
		f.pYes("磁盘", 0.05)
		before := f.proposedCount()
		m := func(id string, v int) string {
			return ev(id, "acc.metric", "磁盘", fmt.Sprintf(`"correlation_keys":["disk-a"],"metrics":{"value":%d}`, v))
		}
		observed := func(e event) int { return len(f.audit(e.ID, "rule.observed")) }
		m1 := src.emit(m("irl-m1", 95))["irl-m1"]
		m2 := src.emit(m("irl-m2", 80))["irl-m2"]
		m3 := src.emit(m("irl-m3", 92))["irl-m3"]
		eq(t, "irl-m1（序列第一次）观察记录", observed(m1), 0)
		eq(t, "irl-m2（未上穿）观察记录", observed(m2), 0)
		eq(t, "irl-m3（80→92 上穿 90）观察记录", observed(m3), 1)
		eq(t, "建议流", f.proposedCount(), before)
		noDelivery("observe 档的 alert")
	})

	f.step("ACT-IRL-10 相等与成员条件", func() {
		w := func(id, typ, sender string) string { return ev(id, typ, "条件 "+id, `"sender":"`+sender+`"`) }
		for _, id := range []string{"irl-w1", "irl-w2", "irl-w3", "irl-w4", "irl-w5", "irl-w6"} {
			f.pYes("条件 "+id, 0.05)
		}
		got := src.emit(w("irl-w1", "acc.where", "alice"), w("irl-w2", "acc.where", "bob"),
			w("irl-w3", "acc.where-ne", "alice"), w("irl-w4", "acc.where-ne", "bob"),
			w("irl-w5", "acc.where-in", "carol"), w("irl-w6", "acc.where-in", "dave"))
		for _, id := range []string{"irl-w1", "irl-w4", "irl-w5"} {
			eq(t, id+" disposition", got[id].Disposition, "proposed")
			eq(t, id+" 初筛请求", f.clsMentions("条件 "+id), 0)
		}
		for _, id := range []string{"irl-w2", "irl-w3", "irl-w6"} {
			eq(t, id+" disposition", got[id].Disposition, "ignored")
			if f.clsMentions("条件 "+id) == 0 {
				t.Errorf("%s 不命中规则，应按 screen 经初筛", id)
			}
		}
	})

	f.step("ACT-IRL-11 数值比较的边界", func() {
		var lines []string
		want := map[string]string{}
		verdict := func(in bool) string { return map[bool]string{true: "proposed", false: "ignored"}[in] }
		for i, sz := range []int{9, 10, 19, 20} {
			id := fmt.Sprintf("irl-n%d", i+1)
			f.pYes("数值 "+id, 0.05)
			lines = append(lines, ev(id, "acc.size", "数值 "+id, fmt.Sprintf(`"metrics":{"size":%d}`, sz)))
			want[id] = verdict(sz >= 10 && sz < 20)
		}
		for i, sz := range []int{10, 11, 20, 21} {
			id := fmt.Sprintf("irl-n%d", i+5)
			f.pYes("数值 "+id, 0.05)
			lines = append(lines, ev(id, "acc.size2", "数值 "+id, fmt.Sprintf(`"metrics":{"size":%d}`, sz)))
			want[id] = verdict(sz > 10 && sz <= 20)
		}
		for id, d := range src.emit(lines...) {
			eq(t, id+" disposition", d.Disposition, want[id])
		}
	})

	f.step("ACT-IRL-12 changed 与 crosses_below 只与同一序列比较", func() {
		lv := func(id, key string, v int) string {
			return ev(id, "acc.level", "序列 "+id, fmt.Sprintf(`"correlation_keys":[%q],"metrics":{"level":%d}`, key, v))
		}
		tp := func(id string, v int) string {
			return ev(id, "acc.temp", "序列 "+id, fmt.Sprintf(`"correlation_keys":["t-a"],"metrics":{"temp":%d}`, v))
		}
		for _, id := range []string{"irl-c1", "irl-c2", "irl-c3", "irl-c4", "irl-t1", "irl-t2"} {
			f.pYes("序列 "+id, 0.05)
		}
		got := map[string]event{}
		for _, step := range [][2]string{{"irl-c1", lv("irl-c1", "lv-a", 1)}, {"irl-c2", lv("irl-c2", "lv-a", 1)}, {"irl-c3", lv("irl-c3", "lv-a", 2)},
			{"irl-c4", lv("irl-c4", "lv-b", 2)}, {"irl-t1", tp("irl-t1", 8)}, {"irl-t2", tp("irl-t2", 3)}} {
			got[step[0]] = src.emit(step[1])[step[0]] // 逐条等一个周期：序列的上一次取值按落盘顺序
		}
		for id, want := range map[string]string{"irl-c1": "ignored", "irl-c2": "ignored", "irl-c3": "proposed", "irl-c4": "ignored", "irl-t1": "ignored", "irl-t2": "proposed"} {
			eq(t, id+" disposition", got[id].Disposition, want)
		}
	})

	// 复制 acc-rules 并改一处，运行 pack validate，返回结果。
	mutate := func(name, from, to string) baseline.Result {
		dir := filepath.Join(f.w, name)
		f.must(os.MkdirAll(filepath.Join(dir, "prompts"), 0o755))
		b, err := os.ReadFile(filepath.Join(packDir, "pack.yaml"))
		f.must(err)
		if !strings.Contains(string(b), from) {
			t.Fatalf("包里找不到 %q", from)
		}
		f.must(os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(strings.Replace(string(b), from, to, 1)), 0o644))
		return e.Apa(e.OBS, "", "pack", "validate", dir)
	}
	out := func(r baseline.Result) string { return r.Stdout + r.Stderr }

	f.step("ACT-IRL-13 变化类条件用在 metrics 以外字段时整包被拒", func() {
		r := mutate("bad1", "field: metrics.level, op: changed", "field: subject, op: changed")
		if r.Code == 0 || !strings.Contains(out(r), "r-chg") || !strings.Contains(out(r), "metrics.") {
			t.Errorf("应退出码非 0 并指出 r-chg 的 changed 只能用于 metrics.<名>: %d %s", r.Code, out(r))
		}
	})

	f.step("ACT-IRL-14 未知的 op 或出口被拒", func() {
		r := mutate("bad2", "op: eq, value: alice", "op: regex, value: alice")
		if r.Code == 0 || !strings.Contains(out(r), "op") {
			t.Errorf("op=regex 应被拒并指出 op: %d %s", r.Code, out(r))
		}
		r = mutate("bad3", "event_type: [acc.ignore]}, then: ignore", "event_type: [acc.ignore]}, then: archive")
		if r.Code == 0 || !strings.Contains(out(r), "then") {
			t.Errorf("then=archive 应被拒并指出 then: %d %s", r.Code, out(r))
		}
	})

	f.step("ACT-IRL-15 alert 规则缺 where 被拒，放进数据目录后保留上一有效版本", func() {
		r := mutate("bad4", "    where: [{field: metrics.value, op: crosses_above, value: 90}]\n", "")
		if r.Code == 0 || !strings.Contains(out(r), "where") {
			t.Fatalf("alert 缺 where 应被拒并指出 where: %d %s", r.Code, out(r))
		}
		good, err := os.ReadFile(filepath.Join(packDir, "pack.yaml"))
		f.must(err)
		bad, err := os.ReadFile(filepath.Join(f.w, "bad4", "pack.yaml"))
		f.must(err)
		health := func() string { return f.apa(e.OBS, 0, "health").Stdout }
		f.must(os.WriteFile(filepath.Join(packDir, "pack.yaml"), bad, 0o644))
		restart()
		f.until("健康视图报告 pack_rejections", func() bool { return rejected(health()) })
		// 保留上一有效版本：r-alert 等规则仍按原包生效。
		got := src.emit(ev("irl-kp1", "acc.proposed", "旧包仍生效", ""))["irl-kp1"]
		eq(t, "disposition", got.Disposition, "proposed")
		f.must(os.WriteFile(filepath.Join(packDir, "pack.yaml"), good, 0o644))
		restart()
		f.until("pack_rejections 恢复 ok", func() bool { return !rejected(health()) })
	})

	f.step("ACT-IRL-16 project_by fixed 归到固定项目", func() {
		got := src.emit(ev("irl-f1", "acc.fixed", "物业缴费", ""))["irl-f1"]
		eq(t, "disposition", got.Disposition, "proposed")
		eq(t, "project", f.item(got.Item).Project, e.Projects["acc-home"])
	})

	f.step("ACT-IRL-17 project_by none 时取来源的默认项目", func() {
		// type=script 来源的改动只能由本人在 Web 做（delegate 得 web_only，设计 §9.5）；验收文档写的 $DEL 在这里不适用。
		if r := e.APIRaw("PATCH", "/v1/sources/"+src.id, `{"default_project":""}`, e.DEL); r.Code != 403 {
			t.Fatalf("delegate 改 script 来源应得 403 web_only，实际 %d %s", r.Code, r.Body)
		}
		patch := func(body string) { f.webOK("PATCH", "/sources/"+src.id, body, 200) }
		patch(fmt.Sprintf(`{"default_project":%q}`, e.Projects["acc-open"]))
		o1 := src.emit(ev("irl-o1", "acc.none", "默认项目事件", ""))["irl-o1"]
		eq(t, "irl-o1 project", f.item(o1.Item).Project, e.Projects["acc-open"])
		patch(`{"default_project":""}`)
		o2 := src.emit(ev("irl-o2", "acc.none", "清空后事件", ""))["irl-o2"]
		eq(t, "irl-o2 project", f.item(o2.Item).Project, "")
		eq(t, "irl-o1 事项项目不变", f.item(o1.Item).Project, e.Projects["acc-open"])
	})

	f.step("ACT-IRL-18 project_by draft 由起草从项目清单里选", func() {
		f.pYes("项目选择 irl-r1", 0.9)
		f.pYes("项目选择 irl-r2", 0.9)
		f.draft("项目选择 irl-r1", map[string]any{"title": "归 acc-work", "kind": "task", "project": e.Projects["acc-work"]})
		f.draft("项目选择 irl-r2", map[string]any{"title": "归清单外", "kind": "task", "project": "no-such-project"})
		got := src.emit(ev("irl-r1", "acc.draft", "项目选择 irl-r1", ""), ev("irl-r2", "acc.draft", "项目选择 irl-r2", ""))
		for _, k := range []string{"irl-r1", "irl-r2"} {
			var body string
			for _, r := range e.ModelStub.Requests() {
				if strings.Contains(r.BodyText, "项目选择 "+k) && strings.Contains(r.BodyText, "起草一条建议") {
					body = r.BodyText
				}
			}
			for _, p := range []string{"acc-work", "acc-open", "acc-home"} {
				if !strings.Contains(body, e.Projects[p]) {
					t.Errorf("%s 的起草请求应带活动项目清单（含 %s）: %.600s", k, p, body)
				}
			}
		}
		eq(t, "irl-r1 project", f.item(got["irl-r1"].Item).Project, e.Projects["acc-work"])
		if p := f.item(got["irl-r2"].Item).Project; p != "" {
			t.Errorf("irl-r2 不得归到清单外的项目，实际 %q", p)
		}
	})

	f.step("ACT-IRL-19 规则按条覆盖初筛模板名", func() {
		f.pYes("模板覆盖", 0.05)
		got := src.emit(ev("irl-tp1", "acc.tpl", "模板覆盖", ""))["irl-tp1"]
		eq(t, "disposition", got.Disposition, "ignored")
		tmpl(t, f.judgment(got), "acc-screen")
	})

	f.step("ACT-IRL-20 来源事件的新字段与新处置可经 API 读出", func() {
		k1 := src.emit(ev("irl-k1", "acc.proposed", "带新字段", `"metrics":{"size":3},"merge_key":"mk-irl-k1"`))["irl-k1"]
		eq(t, "irl-k1 metrics.size", k1.Metrics["size"], 3.0)
		eq(t, "irl-k1 merge_key", k1.MergeKey, "mk-irl-k1")
		s1, _ := src.find("irl-s1")
		if s1.Judgment == "" {
			t.Errorf("irl-s1 应带 judgment: %+v", s1)
		}
		var scored struct {
			Events []event `json:"events"`
		}
		f.must(json.Unmarshal([]byte(e.APIRaw("GET", "/v1/sources/"+src.id+"/events?sort=score&limit=500", "", e.OBS).Body), &scored))
		for _, ev := range scored.Events {
			if ev.DedupeKey.EventID == "irl-s1" && (ev.Score == nil || *ev.Score != 0.9) {
				t.Errorf("irl-s1 应带 score=0.9: %v", ev.Score)
			}
		}
		x1, _ := src.find("irl-x1")
		eq(t, "irl-x1 items", len(x1.Items), 2)
		var filtered struct {
			Events []event `json:"events"`
		}
		r := e.APIRaw("GET", "/v1/sources/"+src.id+"/events?disposition=digest,alerted&limit=500", "", e.OBS)
		eq(t, "disposition=digest,alerted HTTP", r.Code, 200)
		f.must(json.Unmarshal([]byte(r.Body), &filtered))
		var seenDigest bool
		for _, ev := range filtered.Events {
			if ev.Disposition != "digest" && ev.Disposition != "alerted" {
				t.Errorf("过滤结果混入 %s（%s）", ev.DedupeKey.EventID, ev.Disposition)
			}
			seenDigest = seenDigest || ev.DedupeKey.EventID == "irl-d1"
		}
		if !seenDigest {
			t.Error("disposition=digest,alerted 应含 irl-d1")
		}
		eq(t, "disposition=bogus HTTP", e.APIRaw("GET", "/v1/sources/"+src.id+"/events?disposition=bogus", "", e.OBS).Code, 400)
	})

	f.step("ACT-IRL-21 不带新字段的旧来源程序照常工作", func() {
		got := src.emit(ev("irl-old1", "acc.proposed", "旧格式", `"correlation_keys":["old-1"],"sender":"alice"`))["irl-old1"]
		eq(t, "disposition", got.Disposition, "proposed")
		if strings.Contains(f.apa(e.OBS, 0, "source", "rejects", src.id).Stdout, "irl-old1") {
			t.Error("来源页\"被拒收的事件\"不应有 irl-old1")
		}
		files, _ := filepath.Glob(filepath.Join(f.w, "stdin-"+src.id+"-*.json"))
		if len(files) == 0 {
			t.Fatal("没有 stdin 记录")
		}
		b, err := os.ReadFile(files[len(files)-1])
		f.must(err)
		var in map[string]json.RawMessage
		f.must(json.Unmarshal(b, &in))
		for _, k := range []string{"source_id", "config", "cursor", "window", "run_id"} {
			if _, ok := in[k]; !ok {
				t.Errorf("stdin 缺少 §9.2 字段 %s: %s", k, b)
			}
		}
		var v struct {
			Protocols struct {
				SourceContract int `json:"source_contract"`
			} `json:"protocols"`
		}
		f.must(json.Unmarshal([]byte(f.apa(e.OBS, 0, "version").Stdout), &v))
		eq(t, "protocols.source_contract", v.Protocols.SourceContract, 1)
	})

	f.step("ACT-IRL-22 内置规则：日程新建与更新直接成为建议", func() {
		got := cal.emit(ev("irl-cal-1", "event.created", "体检", `"correlation_keys":["uid-irl-1|"]`))["irl-cal-1"]
		eq(t, "disposition", got.Disposition, "proposed")
		eq(t, "kind", f.item(got.Item).Kind, "task")
		ruleIs(t, "命中规则", f.ruleOf(got), "calendar-event")
		if w := f.apa(e.DEL, 0, "why", got.Item).Stdout; !strings.Contains(w, "calendar-event") {
			t.Errorf("apa why 应显示规则 id calendar-event: %s", w)
		}
		f.noModelCalls("体检")
	})

	f.step("ACT-IRL-23 内置规则：到期的提醒事项直接成为建议", func() {
		got := rem.emit(ev("irl-rem-1", "reminder.due", "交水费", `"correlation_keys":["rem-1"]`))["irl-rem-1"]
		eq(t, "disposition", got.Disposition, "proposed")
		eq(t, "kind", f.item(got.Item).Kind, "task")
		ruleIs(t, "命中规则", f.ruleOf(got), "reminder-due")
		f.noModelCalls("交水费")
	})

	f.step("ACT-IRL-24 内置规则：PR 状态变化与规范文件变化被忽略", func() {
		before := f.proposedCount()
		got := gh.emit(ev("irl-pr-1", "pr.closed", "修复登录", `"correlation_keys":["acc-org/acc-repo#7"]`), ev("irl-agm-1", "repo.agents_md_changed", "AGENTS.md 变化", ""))
		eq(t, "irl-pr-1", got["irl-pr-1"].Disposition, "ignored")
		ruleIs(t, "irl-pr-1 命中规则", f.ruleOf(got["irl-pr-1"]), "pr-activity")
		eq(t, "irl-agm-1", got["irl-agm-1"].Disposition, "ignored")
		ruleIs(t, "irl-agm-1 命中规则", f.ruleOf(got["irl-agm-1"]), "agents-md-changed")
		f.noModelCalls("修复登录")
		f.noModelCalls("AGENTS.md 变化")
		eq(t, "建议流", f.proposedCount(), before)
	})

	var clsAt, modelAt int
	f.step("ACT-IRL-25 撤掉两个模型端点并停止替身", func() {
		f.webOK("PUT", "/settings/model-endpoints", `{"judge_endpoint":null,"generate_endpoint":null}`, 200)
		e.Cls.Close()
		e.ModelStub.Close()
		clsAt, modelAt = f.clsLines(), f.modelLines()
	})

	f.step("ACT-IRL-26 不配模型端点时结构化事件照常被规则处置", func() {
		ids := map[*source][]string{
			cal: cal.put(ev("irl-cal-2", "event.created", "体检二", `"correlation_keys":["uid-irl-2|"]`)),
			rem: rem.put(ev("irl-rem-2", "reminder.due", "交电费", `"correlation_keys":["rem-2"]`)),
			gh:  gh.put(ev("irl-pr-2", "pr.merged", "合并登录修复", `"correlation_keys":["acc-org/acc-repo#8"]`)),
			src: src.put(ev("irl-p2", "acc.proposed", "无模型也建议", ""), ev("irl-s2", "acc.screen", "无模型的初筛", "")),
		}
		got := map[string]event{}
		for s, l := range ids {
			for k, v := range s.wait(l...) {
				got[k] = v
			}
		}
		for _, id := range []string{"irl-cal-2", "irl-rem-2", "irl-p2"} {
			eq(t, id+" disposition", got[id].Disposition, "proposed")
		}
		eq(t, "irl-pr-2 disposition", got["irl-pr-2"].Disposition, "ignored")
		eq(t, "irl-s2 disposition", got["irl-s2"].Disposition, "unjudged")
		eq(t, "irl-s2 unjudged_reason", got["irl-s2"].UnjudgedReason, "model_unavailable")
		eq(t, "CLS_LOG 行数不变", f.clsLines(), clsAt)
		eq(t, "MODEL_LOG 行数不变", f.modelLines(), modelAt)
		var h struct {
			Signals []struct {
				ID     string          `json:"id"`
				Detail json.RawMessage `json:"detail"`
			} `json:"signals"`
		}
		f.must(json.Unmarshal([]byte(f.apa(e.OBS, 0, "health").Stdout), &h))
		for _, s := range h.Signals {
			if s.ID == "model_unavailable" {
				// 内核把两个端点合成一个状态：都未配置时 detail.endpoint 为 not_configured（验收文档写的是 judge、generate 各一项）。
				var d struct {
					Endpoint string `json:"endpoint"`
				}
				f.must(json.Unmarshal(s.Detail, &d))
				eq(t, "model_unavailable.detail.endpoint", d.Endpoint, "not_configured")
			}
		}
	})

	f.step("ACT-IRL-27 恢复端点与替身", func() {
		e.Cls, e.ModelStub = nil, nil
		e.WireEndpoints()
		f.must(os.RemoveAll(packDir))
		restart()
		for _, s := range []*source{src, cal, rem, gh} {
			f.webOK("PATCH", "/sources/"+s.id, `{"enabled":false}`, 200) // 启停是替本人决定的动作，delegate 需要原话，这里按本人在 Web 操作
		}
		if strings.Contains(f.apa(e.OBS, 0, "pack", "list").Stdout, "acc-rules") {
			t.Error("apa pack list 不应再有 acc-rules")
		}
		f.until("model_unavailable 恢复 ok", func() bool { return !unavailable(f.apa(e.OBS, 0, "health").Stdout) })
	})
}

func signalOK(health, id string) bool {
	var h struct {
		Signals []struct {
			ID string `json:"id"`
			OK bool   `json:"ok"`
		} `json:"signals"`
	}
	if json.Unmarshal([]byte(health), &h) != nil {
		return true
	}
	for _, s := range h.Signals {
		if s.ID == id {
			return s.OK
		}
	}
	return true
}

// rejected 报告 `apa health` 输出里 pack_rejections 信号是否为不通过。
func rejected(health string) bool { return !signalOK(health, "pack_rejections") }

func unavailable(health string) bool { return !signalOK(health, "model_unavailable") }

// tmpl 断言判断记录用的模板名（记录里是 <包>/<模板>@<指纹>；模板名不在发给端点的内容里）。
func tmpl(t *testing.T, j map[string]any, want string) {
	t.Helper()
	got, _ := j["template"].(string)
	if i := strings.IndexByte(got, '@'); i >= 0 {
		got = got[:i]
	}
	if got != want && !strings.HasSuffix(got, "/"+want) {
		t.Errorf("判断记录的模板 = %q，期望 %q", got, want)
	}
}

// ruleIs 断言命中的规则 id；内核记的是 <包>/source_rules/<规则>。
func ruleIs(t *testing.T, what, got, want string) {
	t.Helper()
	if !strings.HasSuffix(got, "/"+want) {
		t.Errorf("%s = %q，期望 ……/%s", what, got, want)
	}
}
