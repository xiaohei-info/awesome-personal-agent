//go:build apa_testclock

package webhook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
)

// 写偏移后，未注入 Now 的 Service 取到的 Now 前进：超过保留期的原始 payload 立即可清。
func TestTestClockOffsetPastRetentionPurgesPayload(t *testing.T) {
	home := t.TempDir()
	clock.SetHome(home)
	e := newEnv(t, func(o *Options) { o.Now = nil })
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	status(t, e.post("src_hook", "d-1", issueBody(1, "opened"), nil), 200)
	if err := e.svc.Expire(bg); err != nil {
		t.Fatal(err)
	}
	if g := e.ingests()[0]; g.Purged || len(g.Body) == 0 {
		t.Fatalf("payload purged before the retention elapsed: %+v", g)
	}
	if err := os.WriteFile(filepath.Join(home, clock.OffsetFile), []byte("1000h"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Expire(bg); err != nil {
		t.Fatal(err)
	}
	if g := e.ingests()[0]; !g.Purged || len(g.Body) != 0 {
		t.Fatalf("payload kept after the offset passed the retention: %+v", g)
	}
}
