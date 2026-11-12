package packs

import (
	"strings"
	"testing"

	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

const v2Head = "name: x\nversion: 1\ndescription: d\n"

// validate 把一个提示词（可带额外文件）作为本地包预检，返回错误文本（通过为空）。
func validate(t *testing.T, pack string, prompts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"pack.yaml": pack}
	for n, c := range prompts {
		files["prompts/"+n+".md"] = c
	}
	writePack(t, dir, files)
	if _, err := Validate(dir, newKinds().CheckSchema); err != nil {
		return err.Error()
	}
	return ""
}

func judge(front string) map[string]string {
	return map[string]string{"j": "---\nagent: judge\n" + front + "---\nq {item_id}\n"}
}

// ACT-PACK-07：已移除的字段被拒绝，错误信息给出 C.7 的新写法。
func TestRemovedFieldsRejectedWithNewSpelling(t *testing.T) {
	cases := []struct {
		name    string
		prompts map[string]string
		want    []string
	}{
		{"confidence_threshold", judge("question_type: yes_no\non_undecided: yes\nconfidence_threshold: 0.7\n"), []string{"confidence_threshold", "band", "answer_by"}},
		{"judge escalate_to", judge("question_type: yes_no\non_undecided: yes\nescalate_to: x\n"), []string{"escalate_to", "answer_by", "rule"}},
		{"generate escalate_to", map[string]string{"g": "---\nagent: generate\nescalate_to: y\noutput_schema: {type: object}\n---\nq\n"}, []string{"escalate_to", "relay_to"}},
		{"choice with need-explanation and no on_undecided", judge("question_type: choice\nchoices: [remind, 需要解释]\n"), []string{"on_undecided", "需要解释"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validate(t, v2Head, c.prompts)
			if got == "" {
				t.Fatal("pack with removed fields was accepted")
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("error %q lacks %q", got, w)
				}
			}
		})
	}
}

// ACT-PACK-08：判断型新字段的组合。
func TestJudgeFieldCombinations(t *testing.T) {
	bad := []struct{ name, front, want string }{
		{"missing on_undecided", "question_type: yes_no\n", "on_undecided"},
		{"on_undecided maybe", "question_type: yes_no\non_undecided: maybe\n", "on_undecided"},
		{"choice on_undecided outside choices", "question_type: choice\nchoices: [a, b]\non_undecided: c\n", "on_undecided"},
		{"score on_undecided not a number", "question_type: score\non_undecided: high\n", "on_undecided"},
		{"classifier on choice", "question_type: choice\nchoices: [a]\non_undecided: a\nanswer_by: [classifier]\n", "classifier"},
		{"unknown backend", "question_type: yes_no\non_undecided: yes\nanswer_by: [mystery]\n", "mystery"},
		{"empty answer_by", "question_type: yes_no\non_undecided: yes\nanswer_by: []\n", "answer_by"},
		{"band low>high", "question_type: yes_no\non_undecided: yes\nband: {low: 0.7, high: 0.3}\n", "band"},
		{"band out of range", "question_type: yes_no\non_undecided: yes\nband: {low: 1.2}\n", "band"},
		{"empty band", "question_type: yes_no\non_undecided: yes\nband: {}\n", "band"},
		{"band without classifier", "question_type: yes_no\non_undecided: yes\nanswer_by: [chat]\nband: {low: 0.1}\n", "band"},
		{"single_shot without classifier", "question_type: yes_no\non_undecided: yes\nanswer_by: [chat]\nsingle_shot: true\n", "single_shot"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			got := validate(t, v2Head, judge(c.front))
			if got == "" || !strings.Contains(got, c.want) {
				t.Fatalf("want rejection mentioning %q, got %q", c.want, got)
			}
		})
	}
	ok := "question_type: yes_no\nanswer_by: [classifier, chat]\nband: {low: 0.15, high: 0.60}\non_undecided: yes\ncriteria: 需要本人做事的算。\n"
	if got := validate(t, v2Head, judge(ok)); got != "" {
		t.Fatalf("compliant template rejected: %s", got)
	}
}

// 缺省与单端 band：yes_no 缺省 [classifier, chat]，其余 [chat]；只写一端时另一端取相同的值。
func TestJudgeDefaultsAndOneSidedBand(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, map[string]string{
		"pack.yaml":        v2Head,
		"prompts/yn.md":    "---\nagent: judge\nquestion_type: yes_no\nband: {low: 0.2}\non_undecided: \"no\"\n---\nq\n",
		"prompts/ch.md":    "---\nagent: judge\nquestion_type: choice\nchoices: [a, b]\non_undecided: b\n---\nq\n",
		"prompts/score.md": "---\nagent: judge\nquestion_type: score\non_undecided: 3\n---\nq\n",
	})
	p, err := Validate(dir, newKinds().CheckSchema)
	if err != nil {
		t.Fatal(err)
	}
	yn, ch := p.Prompts["yn"].Meta, p.Prompts["ch"].Meta
	if strings.Join(yn.AnswerBy, ",") != "classifier,chat" || strings.Join(ch.AnswerBy, ",") != "chat" {
		t.Errorf("answer_by defaults: %v %v", yn.AnswerBy, ch.AnswerBy)
	}
	if *yn.Band.Low != 0.2 || *yn.Band.High != 0.2 {
		t.Errorf("one-sided band = %v %v", *yn.Band.Low, *yn.Band.High)
	}
}

// ACT-PACK-09：relay_to 只能由生成型指向任务型。
func TestRelayToOnlyGenerateToTask(t *testing.T) {
	gen := func(relay string) string {
		return "---\nagent: generate\nrelay_to: " + relay + "\noutput_schema: {type: object}\n---\nq\n"
	}
	task := "---\nagent: task\n---\nt\n"
	cases := []struct {
		name    string
		prompts map[string]string
		want    string // 空表示应通过
	}{
		{"judge with relay_to", judge("question_type: yes_no\non_undecided: yes\nrelay_to: t\n"), "relay_to"},
		{"generate to generate", map[string]string{"g": gen("g2"), "g2": gen("t"), "t": task}, "task"},
		{"generate to missing", map[string]string{"g": gen("nope")}, "nope"},
		{"task with relay_to", map[string]string{"t": "---\nagent: task\nrelay_to: t\n---\nt\n"}, "relay_to"},
		{"generate to task", map[string]string{"g": gen("t"), "t": task}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validate(t, v2Head, c.prompts)
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

// ACT-PACK-10：规则自报类别、不确定带策略覆盖、外发范围、打扰上限都属于策略字段，整包拒绝。
func TestPolicyDeclarationsRejected(t *testing.T) {
	for name, y := range map[string]string{
		"category":        "clock_rules:\n  - {id: r, type: follow_up, category: reminder}\n",
		"judgment bands":  "judgment:\n  bands: {x: [0.1, 0.2]}\n",
		"outbound scope":  "source_rules:\n  - {id: r, outbound: all}\n",
		"disturb ceiling": "disturb:\n  daily_cap: 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := validate(t, v2Head+y, nil); !strings.Contains(got, "policy") {
				t.Fatalf("want a policy-field rejection, got %q", got)
			}
		})
	}
}

// ACT-FUP-12：follow_up / stale 规则带 do[] 被拒绝并提示删除；remind_rounds < 1 被拒。
func TestReminderRulesRejectDo(t *testing.T) {
	do := "    do: [{action: call_agent, prompt: p}]\n"
	cases := []struct{ name, rules, want string }{
		{"follow_up with do", "clock_rules:\n  - id: follow-up-interval\n    type: follow_up\n    schedule: 72h\n" + do, "delete do[]"},
		{"stale with do", "clock_rules:\n  - id: s\n    type: stale\n    schedule: 24h\n" + do, "delete do[]"},
		{"remind_rounds 0", "clock_rules:\n  - {id: f, type: follow_up, schedule: 72h, remind_rounds: 0}\n", "remind_rounds"},
		{"remind_rounds on recur", "clock_rules:\n  - {id: f, type: recur, schedule: 72h, remind_rounds: 2}\n", "follow_up"},
		{"stale_after on follow_up", "clock_rules:\n  - {id: f, type: follow_up, schedule: 72h, stale_after: 14d}\n", "stale_after"},
		{"stale_after garbage", "clock_rules:\n  - {id: f, type: stale, schedule: 24h, stale_after: soon}\n", "stale_after"},
		{"defer_with missing prompt", "clock_rules:\n  - {id: f, type: follow_up, schedule: 72h, defer_with: nope}\n", "defer_with"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prompts := map[string]string{"p": "---\nagent: task\n---\nt\n"}
			got := validate(t, v2Head+c.rules, prompts)
			if got == "" || !strings.Contains(got, c.want) {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
	good := "clock_rules:\n  - {id: f, type: follow_up, schedule: 72h, remind_rounds: 2, defer_with: d}\n"
	prompts := map[string]string{"d": "---\nagent: judge\nquestion_type: yes_no\non_undecided: \"no\"\n---\nq\n"}
	if got := validate(t, v2Head+good, prompts); got != "" {
		t.Fatalf("valid follow_up rule rejected: %s", got)
	}
}

// ACT-PACK-11 / ACT-PACK-12 / ACT-UPG-11 / ACT-FUP-12（加载器层）：v0.1 写法的用户覆盖包整包被拒，
// 同名内置包生效，健康视图逐字段列出；修正后重新生效，拒绝信号消失。
func TestV01OverridePackRejectedBuiltinWinsThenRepaired(t *testing.T) {
	data := t.TempDir()
	v01 := "---\nagent: judge\nquestion_type: yes_no\nconfidence_threshold: 0.7\nescalate_to: triage-draft\n---\n这条值得看吗 {item_id}\n"
	writePack(t, data, map[string]string{
		"inbound-triage/pack.yaml":                 mustBuiltin(t, "inbound-triage/pack.yaml"),
		"inbound-triage/prompts/triage.md":         v01,
		"inbound-triage/prompts/triage-draft.md":   mustBuiltin(t, "inbound-triage/prompts/triage-draft.md"),
		"inbound-triage/prompts/triage-extract.md": mustBuiltin(t, "inbound-triage/prompts/triage-extract.md"),
	})
	var reported []Rejection
	l, rej := load(t, Options{Builtin: rootpacks.FS, DataDir: data, Kinds: newKinds(), OnReject: func(r Rejection) { reported = append(reported, r) }})
	if len(rej) != 1 || len(reported) != 1 {
		t.Fatalf("one health delivery expected, got %d rejections / %d reports", len(rej), len(reported))
	}
	if src, _ := l.Source("inbound-triage"); src != Builtin {
		t.Fatalf("effective source = %v, want builtin", src)
	}
	h := l.Health()
	if len(h) != 1 {
		t.Fatalf("health = %+v", h)
	}
	for _, w := range []string{"confidence_threshold", "escalate_to", "on_undecided", "band", "answer_by"} {
		if !strings.Contains(h[0].Reason, w) {
			t.Errorf("health reason lacks %q: %s", w, h[0].Reason)
		}
	}
	// 再次热加载同一内容：不重复产出健康投递。
	if again, _ := l.Load(); len(again) != 0 {
		t.Fatalf("same rejected content reported again: %+v", again)
	}

	// 按提示改成新写法并改 band，重新生效。
	fixed := "---\nagent: judge\nquestion_type: yes_no\nanswer_by: [classifier, chat]\nband: {low: 0.10, high: 0.50}\non_undecided: yes\n---\n这条值得看吗 {item_id}\n"
	writePack(t, data, map[string]string{"inbound-triage/prompts/triage.md": fixed})
	if _, err := l.Load(); err != nil {
		t.Fatal(err)
	}
	if len(l.Health()) != 0 {
		t.Fatalf("rejection signal not cleared: %+v", l.Health())
	}
	p, _ := l.Get("inbound-triage")
	if src, _ := l.Source("inbound-triage"); src != Data || *p.Prompts["triage"].Meta.Band.Low != 0.10 || *p.Prompts["triage"].Meta.Band.High != 0.50 {
		t.Fatalf("repaired override not effective: %v %+v", src, p.Prompts["triage"].Meta.Band)
	}
}

// ACT-FUP-12：覆盖包里 follow-up-interval 带 do[]（v0.1 写法）整包被拒，内置 follow-up-interval（72h）生效。
func TestFollowUpOverrideWithDoRejectedBuiltinEffective(t *testing.T) {
	data := t.TempDir()
	writePack(t, data, map[string]string{
		"follow-up/pack.yaml":                  "name: follow-up\nversion: 1\ndescription: d\nclock_rules:\n  - id: follow-up-interval\n    type: follow_up\n    schedule: 72h\n    do: [{action: call_agent, prompt: follow-up-defer}]\n",
		"follow-up/prompts/follow-up-defer.md": mustBuiltin(t, "follow-up/prompts/follow-up-defer.md"),
	})
	l, rej := load(t, Options{Builtin: rootpacks.FS, DataDir: data, Kinds: newKinds()})
	if len(rej) != 1 || !strings.Contains(rej[0].Reason, "follow-up-interval") || !strings.Contains(rej[0].Reason, "delete do[]") {
		t.Fatalf("rejections = %+v", rej)
	}
	p, ok := l.Get("follow-up")
	if !ok {
		t.Fatal("builtin follow-up not effective")
	}
	var found bool
	for _, r := range p.ClockRules {
		if r.ID == "follow-up-interval" {
			found = r.Schedule == "72h" && len(r.Do) == 0 && *r.RemindRounds == 2 && r.DeferWith == "follow-up-defer"
		}
	}
	if !found {
		t.Fatalf("builtin follow-up-interval wrong: %+v", p.ClockRules)
	}
}

// REQ-G1-5：内置包的判断型模板按新字段改写（C.14）。入站模板由 inbound-pipeline-v2 另行改写，这里只守住它们仍可加载。
func TestBuiltinJudgeTemplatesUseV2Fields(t *testing.T) {
	l, rej := load(t, Options{Builtin: rootpacks.FS, Kinds: newKinds()})
	if len(rej) != 0 {
		t.Fatal(rej)
	}
	type want struct {
		pack, name  string
		low, high   float64
		onUndecided string
		singleShot  bool
	}
	for _, w := range []want{
		{"decision-card", "delivery-defer", 0.85, 0.85, "no", true},
		{"decision-card", "web-only-screening", 0.20, 0.20, "yes", true},
		{"follow-up", "follow-up-defer", 0.85, 0.85, "no", false},
		{"delegation-verify", "delegation-screen", 0.20, 0.20, "yes", false},
	} {
		p, _ := l.Get(w.pack)
		m := p.Prompts[w.name].Meta
		if m.Agent != "judge" || m.QuestionType != "yes_no" || m.Band == nil || *m.Band.Low != w.low || *m.Band.High != w.high ||
			m.OnUndecided != w.onUndecided || m.SingleShot != w.singleShot || m.Criteria == "" || strings.Join(m.AnswerBy, ",") != "classifier,chat" {
			t.Errorf("%s/%s = %+v", w.pack, w.name, m)
		}
	}
	for _, p := range l.List() {
		for n, pr := range p.Prompts {
			if pr.Meta.ConfidenceThreshold != nil || pr.Meta.EscalateTo != "" {
				t.Errorf("%s/%s still carries v0.1 fields", p.Name, n)
			}
		}
	}
}

func mustBuiltin(t *testing.T, name string) string {
	t.Helper()
	b, err := rootpacks.FS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
