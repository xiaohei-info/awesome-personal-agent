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
	if d.MainSession == nil || d.MainSession.Launch != "headless" {
		t.Fatalf("main_session must be headless: %+v", d.MainSession)
	}
}
