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
	if d.Harness != "hermes" || h.Session.Extract != "$.session_id" || h.FinalText != "$.text" || len(h.Resume) == 0 {
		t.Fatalf("unexpected declaration: %+v", d)
	}
	for _, tier := range []string{"read", "edit", "full"} {
		if h.Access[tier] == nil || len(h.Tools[tier]) == 0 {
			t.Errorf("tier %s missing flags or tools", tier)
		}
	}
	// The tool list shown to the owner must match the -t toolsets: edit/full grant a terminal.
	if d.Name == nil || d.Name.Zh != "Hermes" || len(h.Tools["edit"]) != 5 || h.Tools["edit"][3].Zh != "终端" || h.ToolNotes["full"].Zh == "" {
		t.Fatalf("name/tools for the Agent card: %+v %+v %+v", d.Name, h.Tools, h.ToolNotes)
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
	// The file states no wake: the plugin fills main_session from apa-wake's readiness, never a launch choice.
	if d.MainSession == nil || d.MainSession.CanWake() || d.MainSession.Launch != "" {
		t.Fatalf("main_session must declare no wake and no launch choice: %+v", d.MainSession)
	}
}
