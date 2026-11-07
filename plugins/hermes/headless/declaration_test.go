package headless_test

import (
	"os"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
)

// The shipped declaration must be accepted by the kernel's own parser, with the tiers and session/resume the kernel uses.
func TestDeclarationIsAcceptedByKernel(t *testing.T) {
	data, err := os.ReadFile("declaration.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := executors.ParseDeclaration(data)
	if err != nil {
		t.Fatal(err)
	}
	h := d.Headless
	if d.Harness != "hermes" || h.Session.Extract != "$.session_id" || len(h.Resume) == 0 {
		t.Fatalf("unexpected declaration: %+v", d)
	}
	for _, tier := range []string{"read", "edit", "full"} {
		if h.Access[tier] == nil {
			t.Errorf("tier %s missing", tier)
		}
	}
	// Hermes exits 1 on every failure; only the measured "No session found" text marks a lost session.
	lost := map[string]struct {
		code   int
		stderr string
		want   bool
	}{
		"deleted or unknown session": {1, "Error: No session found matching 'local-0f3a'. Use 'hermes sessions list' to see available sessions", true},
		"model endpoint unreachable": {1, "custom didn't respond in time on any of 3 attempts\n\nProvider said: Connection error.", false},
		"storage error":              {1, "Error: storage_unavailable", false},
		"interrupted":                {130, "No session found matching 'x'", false},
	}
	for name, c := range lost {
		if got := h.Means("session_missing", c.code, nil, []byte(c.stderr)); got != c.want {
			t.Errorf("%s: session_missing = %v, want %v", name, got, c.want)
		}
	}
	if d.MainSession == nil || d.MainSession.Launch != "headless" {
		t.Fatalf("main_session must be headless: %+v", d.MainSession)
	}
}
