//go:build apa_testclock

package runidentity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 写偏移后，本包默认取到的 Now 前进：未显式注入 Now 的 Identity 立即把过期的运行凭据判为未认证。
func TestTestClockOffsetExpiresRunCredentialImmediately(t *testing.T) {
	home := t.TempDir()
	clock.SetHome(home)
	e := setup(t)
	target := e.newItem(t, map[string]any{})
	var runID, secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Launch: LaunchWake, Target: target, TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{"true"}}}, clock.Now())
		if err != nil {
			return err
		}
		runID = r.ID
		secret, _, err = Exchange(tx, command.Caller{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "gateway", Via: domain.ViaCLI}, Kind: auth.Worker}, runID, clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	update := map[string]any{"id": target, "summary": "s"}
	e.must(t, e.as(secret), "item.update", update) // 60 秒有效期内

	if err := os.WriteFile(filepath.Join(home, clock.OffsetFile), []byte("2m"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.want(t, domain.ExitUnauthenticated, e.as(secret), "item.update", update)
}
