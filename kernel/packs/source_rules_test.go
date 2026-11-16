package packs

import (
	"strings"
	"testing"
)

func rulePack(rule string) string {
	return v2Head + "source_rules:\n  - id: r\n    match: {source_type: s}\n" + rule
}

// ACT-IRL-06/13/14/15、ACT-DGA-03（附录 C.11）：来源规则的出口、where[] 与 project_by 校验。
func TestSourceRuleExitsAndWhereValidation(t *testing.T) {
	ok := []string{
		"    then: proposed\n", "    then: ignore\n", "    then: screen\n    screen: acc-screen\n", "    then: judge\n",
		"    then: extract\n", "    then: digest\n", "    project_by: draft\n    then: screen\n", "    project_by: none\n",
		"    where: [{field: sender, op: eq, value: alice}]\n",
		"    where: [{field: sender, op: in, value: [bob, carol]}]\n",
		"    where: [{field: metrics.size, op: gte, value: 10}, {field: metrics.size, op: lt, value: 20}]\n",
		"    where: [{field: metrics.level, op: changed}]\n",
		"    then: alert\n    mode: observe\n    where: [{field: metrics.value, op: crosses_above, value: 90}]\n",
	}
	for _, r := range ok {
		if got := validate(t, rulePack(r), nil); got != "" {
			t.Errorf("%q rejected: %s", r, got)
		}
	}
	bad := []struct{ rule, want string }{
		{"    then: archive\n", "then"},
		{"    then: alert\n", "alert rule must carry where[]"},
		{"    where: [{field: sender, op: regex, value: a}]\n", "op"},
		{"    where: [{field: subject, op: changed}]\n", "changed can only be used on metrics.<name>"},
		{"    where: [{field: sender, op: crosses_below, value: 5}]\n", "metrics.<name>"},
		{"    where: [{field: metrics.v, op: changed, value: 1}]\n", "takes no value"},
		{"    where: [{field: metrics.v, op: crosses_above}]\n", "needs a numeric value"},
		{"    where: [{field: subject, op: gt, value: 1}]\n", "metrics.<name>"},
		{"    where: [{field: metrics.v, op: gt, value: big}]\n", "metrics.<name>"},
		{"    where: [{field: sender, op: in, value: bob}]\n", "list"},
		{"    where: [{field: colour, op: eq, value: red}]\n", "standard event field"},
		{"    project_by: nowhere\n", "project_by"},
	}
	for _, c := range bad {
		got := validate(t, rulePack(c.rule), nil)
		if got == "" || !strings.Contains(got, c.want) {
			t.Errorf("%q: error %q, want it to contain %q", c.rule, got, c.want)
		}
	}
}

// ACT-XAL-09（附录 C.11）：notify_urgent 只用于 screen、extract 出口，用在其他出口上整包拒绝并指出该字段。
func TestNotifyUrgentOnlyOnScreenAndExtract(t *testing.T) {
	for _, then := range []string{"screen", "judge", "extract"} {
		if got := validate(t, rulePack("    then: "+then+"\n    notify_urgent: true\n"), nil); got != "" {
			t.Errorf("then %s + notify_urgent rejected: %s", then, got)
		}
	}
	for _, rule := range []string{"    then: proposed\n", "    then: ignore\n", "    then: digest\n", "    then: alert\n    where: [{field: sender, op: eq, value: a}]\n", ""} {
		got := validate(t, rulePack(rule+"    notify_urgent: true\n"), nil)
		if !strings.Contains(got, "notify_urgent") {
			t.Errorf("%q: error %q should name notify_urgent", rule, got)
		}
	}
	if got := validate(t, rulePack("    then: screen\n    notify_urgent: true\n    mode: auto\n"), nil); !strings.Contains(got, "auto") {
		t.Errorf("notify_urgent with mode auto: error %q", got)
	}
}

// ACT-XAL-09（附录 C.11）：prescreen: none 只用于 extract 出口；包里不能有名为 none 的提示词模板。
func TestPrescreenNoneValidation(t *testing.T) {
	if got := validate(t, rulePack("    then: extract\n    prescreen: none\n"), nil); got != "" {
		t.Errorf("extract + prescreen none rejected: %s", got)
	}
	for _, then := range []string{"screen", "proposed", "ignore", "digest"} {
		got := validate(t, rulePack("    then: "+then+"\n    prescreen: none\n"), nil)
		if !strings.Contains(got, "prescreen") {
			t.Errorf("then %s: error %q should name prescreen", then, got)
		}
	}
	got := validate(t, v2Head, map[string]string{"none": "---\nagent: generate\noutput_schema: {type: object}\n---\nq\n"})
	if !strings.Contains(got, "none") {
		t.Errorf("template named none: error %q", got)
	}
}
