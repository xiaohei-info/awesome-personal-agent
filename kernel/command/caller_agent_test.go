package command

import "testing"

func TestCallerAgentLegacyFallback(t *testing.T) {
	for _, c := range []struct {
		in   Caller
		want string
	}{
		{Caller{CredentialAgentID: "agent_1", CredentialHarness: "hermes"}, "agent_1"},
		{Caller{CredentialHarness: "hermes"}, "legacy:hermes"},
		{Caller{}, ""},
	} {
		if got := c.in.Agent(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.in, got, c.want)
		}
	}
}
