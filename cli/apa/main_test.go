package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
)

type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("help reached the kernel: %s %s", r.Method, r.URL)
	return nil, http.ErrHandlerTimeout
}

// 每个子命令的 -h / --help：退出码 0、用法到 stdout、不碰内核。
func TestHelpOnEveryCommand(t *testing.T) {
	run := func(args ...string) (int, string, string) {
		var out, errw bytes.Buffer
		code := client.Main(args, client.Env{Getenv: func(k string) string {
			return map[string]string{"APA_URL": "http://127.0.0.1:1", "APA_TOKEN": "t", "APA_HOME": "/nonexistent"}[k]
		}, Stdout: &out, Stderr: &errw, HTTP: &http.Client{Transport: noNetwork{t}}})
		return code, out.String(), errw.String()
	}
	_, _, none := run() // 无参数时的错误体列出全部子命令
	var body struct{ Next string }
	if err := json.Unmarshal([]byte(none), &body); err != nil {
		t.Fatal(err)
	}
	_, names, ok := strings.Cut(body.Next, "available commands: ")
	if !ok {
		t.Fatalf("no command list: %q", body.Next)
	}
	for _, n := range strings.Split(names, ", ") {
		for _, h := range []string{"-h", "--help"} {
			if code, out, errw := run(n, h); code != 0 || !strings.HasPrefix(out, "usage: apa ") || errw != "" {
				t.Errorf("apa %s %s: code %d out %q err %q", n, h, code, out, errw)
			}
		}
	}
	for args, want := range map[string]string{"source propose --help": "propose <event-id>", "item update --help": "apa item update <id>", "propose --help": "apa propose --kind",
		"source --help": "attach <event-id>", "item update itm_1 --help": "--summary"} {
		if code, out, _ := run(strings.Fields(args)...); code != 0 || !strings.Contains(out, want) {
			t.Errorf("apa %s: code %d out %q", args, code, out)
		}
	}
}
