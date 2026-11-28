package executors

import (
	"encoding/json"
	"slices"
	"testing"
)

// 执行者参数与可覆盖参数（附录 A.2）：取值只来自句柄与声明列出的 choices；单次覆盖 > 执行者默认 > 不传；
// 命令模板里有 {executor_args} 时放在那里（子命令之前的全局参数），没有就追加在后面。
func TestLaunchArgsComeFromHandleAndDeclaredChoices(t *testing.T) {
	d, err := ParseDeclaration([]byte(`{"harness": "hx",
		"executor_args": {"profile": ["-p", "{profile}"]},
		"overrides": {"model": {"label": {"zh": "模型", "en": "Model"}, "args": ["-m", "{model}"], "choices": ["m1", "m2"], "role": "model"}},
		"headless": {"command": ["prog", "{executor_args}", "chat", "-q", "{prompt}"], "session": {"extract": "$.s"}, "access": {"edit": []}},
		"executors": [{"id": "hx.coder", "kind": "external-cli", "handle": {"profile": "coder"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := Entry{Executor: d.Executors[0], Overrides: map[string]string{"model": "m1"}}
	if _, err := d.LaunchFor(e, map[string]string{"model": "anything"}); err == nil {
		t.Fatal("a value outside choices must be refused")
	}
	l, err := d.LaunchFor(e, nil)
	if err != nil || l.Model != "m1" || !slices.Equal(l.Args, []string{"-p", "coder", "-m", "m1"}) {
		t.Fatalf("executor default: %+v %v", l, err)
	}
	if l, _ = d.LaunchFor(e, map[string]string{"model": "m2"}); l.Model != "m2" {
		t.Fatalf("single delegation wins: %+v", l)
	}
	argv := Expand(d.Headless.Command, map[string]string{"{prompt}": "do it"}, l.Args)
	if !slices.Equal(argv, []string{"prog", "-p", "coder", "-m", "m2", "chat", "-q", "do it"}) {
		t.Fatalf("argv = %q", argv)
	}
	if argv := Expand([]string{"prog", "{prompt}"}, map[string]string{"{prompt}": "x"}, []string{"-p", "c"}); !slices.Equal(argv, []string{"prog", "x", "-p", "c"}) {
		t.Fatalf("appended without a marker: %q", argv)
	}
	for _, bad := range []string{
		`{"harness":"b","headless":{"command":["{executor_args}","x"],"session":{"extract":"$.s"},"access":{"edit":[]}}}`,
		`{"harness":"b","headless":{"command":["p","a{executor_args}"],"session":{"extract":"$.s"},"access":{"edit":[]}}}`,
		`{"harness":"b","overrides":{"model":{"label":{"zh":"m","en":"m"},"args":["-m","{model}"],"choices":[]}},"headless":{"command":["p"],"session":{"extract":"$.s"},"access":{"edit":[]}}}`,
		`{"harness":"b","executor_args":{"profile":["-p","free text"]},"headless":{"command":["p"],"session":{"extract":"$.s"},"access":{"edit":[]}}}`,
	} {
		if _, err := ParseDeclaration(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
