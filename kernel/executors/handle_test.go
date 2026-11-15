package executors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 句柄只看结构（§6.17）：每条规则一条拒绝用例，why 不回显取值。
func TestValidateHandleRejectsCommandsPromptsAndCredentials(t *testing.T) {
	long := strings.Repeat("a", 257)
	deep := `{"mech":"x","a":{"b":{"c":{"d":"e"}}}}`
	many := map[string]string{"mech": "x"}
	for i := 0; i < 32; i++ {
		many[string(rune('a'+i%26))+strings.Repeat("k", i/26+1)] = "v"
	}
	manyJSON, _ := json.Marshal(many)
	cases := map[string]string{
		"not object":         `["mech"]`,
		"key format":         `{"Mech":"x"}`,
		"command key":        `{"mech":"x","command":"rm -rf /"}`,
		"argv segment":       `{"mech":"x","run_argv":["a"]}`,
		"prompt key":         `{"mech":"x","initial_prompt":"do it"}`,
		"env key":            `{"mech":"x","env":{"A":"b"}}`,
		"token key":          `{"mech":"x","access_token":"abc"}`,
		"api key pair":       `{"mech":"x","api_key":"abc"}`,
		"newline":            `{"mech":"x","note":"a\nb"}`,
		"too long string":    `{"mech":"x","note":"` + long + `"}`,
		"bearer":             `{"mech":"x","h":"Bearer abc"}`,
		"pem":                `{"mech":"x","h":"-----BEGIN PRIVATE KEY-----"}`,
		"jwt":                `{"mech":"x","h":"eyJhbGciOi.eyJzdWIiOi.sig"}`,
		"url userinfo":       `{"mech":"x","u":"https://u:p@host/x"}`,
		"url token param":    `{"mech":"x","u":"https://host/x?token=abc"}`,
		"apa credential":     `{"mech":"x","h":"apa_dlg_0123456789"}`,
		"apa run credential": `{"mech":"x","h":"apa_run_0123456789"}`,
		"too deep":           deep,
		"too many keys":      string(manyJSON),
	}
	for name, in := range cases {
		_, err := ValidateHandle("handle", json.RawMessage(in))
		de, ok := err.(*domain.Error)
		if !ok || de.ExitCode() != domain.ExitUsage {
			t.Errorf("%s: want exit 2, got %v", name, err)
			continue
		}
		for _, secret := range []string{"rm -rf", "abc", "do it", "Bearer", "BEGIN", "0123456789"} {
			if strings.Contains(de.Why, secret) {
				t.Errorf("%s: why echoes the value: %s", name, de.Why)
			}
		}
	}
	big := `{"mech":"x","l":[` + strings.TrimSuffix(strings.Repeat(`"`+strings.Repeat("b", 250)+`",`, 9), ",") + `]}`
	if _, err := ValidateHandle("handle", json.RawMessage(big)); err == nil {
		t.Error("handles over 2 KiB must be rejected")
	}
}

func TestValidateHandleKeepsAValidHandleAsWritten(t *testing.T) {
	in := `{ "mech": "paseo", "host": "mbp", "agent_id": "agt_1", "cwd": "/w/app", "n": 3 }`
	out, err := ValidateHandle("handle", json.RawMessage(in))
	if err != nil || string(out) != `{"mech":"paseo","host":"mbp","agent_id":"agt_1","cwd":"/w/app","n":3}` {
		t.Fatalf("%s %v", out, err)
	}
	if out, err := ValidateHandle("handle", nil); err != nil || out != nil {
		t.Fatalf("empty handle is allowed: %s %v", out, err)
	}
}
