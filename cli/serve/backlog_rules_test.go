package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// ACT-UPG-09、ACT-BKL-02～04：升级后的合成账本上，积压原样可列（旧原因不改写）；缺本人原话被拒、积压不变；
// 带本人原话的第 1 步只跑规则：挂靠、按规则出口处置的条数与清单一致，其余原样，且不外呼（模型端点未配置，
// 若走了判断就会把原因改写成 model_unavailable）。
func TestBacklogRulesOnUpgradedFixture(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	m, err := ledgerfixture.Generate(home, 3)
	if err != nil {
		t.Fatal(err)
	}
	cfg := "listen: 127.0.0.1:" + strconv.Itoa(freePort(t)) + "\nunjudged_rejudge_days: 0\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, stop := start(t, home)
	defer stop()
	base := "http://" + addr.String() + "/v1"

	do := func(method, path, token string, hdr map[string]string, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	// 积压按处置与原因计数（所有来源）。
	count := func() map[string]int {
		t.Helper()
		code, b := do("GET", "/sources", m.Tokens["observer"], nil, "")
		var srcs struct{ Sources []struct{ ID string } }
		if code != 200 || json.Unmarshal(b, &srcs) != nil {
			t.Fatalf("sources: %d %s", code, b)
		}
		out := map[string]int{}
		for _, s := range srcs.Sources {
			code, b := do("GET", "/sources/"+s.ID+"/events?disposition=unjudged,expired&limit=500", m.Tokens["observer"], nil, "")
			var evs struct {
				Events []struct {
					Disposition    string `json:"disposition"`
					UnjudgedReason string `json:"unjudged_reason"`
				}
			}
			if code != 200 || json.Unmarshal(b, &evs) != nil {
				t.Fatalf("events %s: %d %s", s.ID, code, b)
			}
			for _, e := range evs.Events {
				out[fmt.Sprintf("%s/%s", e.Disposition, e.UnjudgedReason)]++
			}
		}
		return out
	}
	total := func(c map[string]int) (n int) {
		for _, v := range c {
			n += v
		}
		return
	}

	before := count()
	if total(before) != m.Backlog.Total || before["unjudged/undecided"] != m.Backlog.Unjudged["undecided"] || before["unjudged/"] != m.Backlog.Unjudged["none"] {
		t.Fatalf("listed backlog %v does not match manifest %+v", before, m.Backlog)
	}

	direct := map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct"}
	withWords := map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct", "X-APA-Evidence": "本人：先只跑规则"}
	group := map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "group", "X-APA-Evidence": "本人：先只跑规则"}
	for name, c := range map[string]struct {
		token string
		hdr   map[string]string
		want  int
	}{"no words": {m.Tokens["delegate"], direct, 403}, "group": {m.Tokens["delegate"], group, 403}, "observer": {m.Tokens["observer"], withWords, 403}} {
		if code, b := do("POST", "/source-events/rules", c.token, c.hdr, `{}`); code != c.want || !bytes.Contains(b, []byte(`"code"`)) {
			t.Fatalf("%s: %d %s", name, code, b)
		}
	}
	if got := count(); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("refused calls changed the backlog: %v -> %v", before, got)
	}

	code, b := do("POST", "/source-events/rules", m.Tokens["delegate"], withWords, `{}`)
	var res struct{ Attached, Disposed, Remaining int }
	if code != 200 || json.Unmarshal(b, &res) != nil {
		t.Fatalf("rules: %d %s", code, b)
	}
	if res.Attached != m.Attachable || res.Disposed != m.StructuredHits || res.Remaining != m.Backlog.Total-m.Attachable-m.StructuredHits {
		t.Fatalf("rules result %+v, manifest attachable=%d structured=%d total=%d", res, m.Attachable, m.StructuredHits, m.Backlog.Total)
	}
	after := count()
	if total(after) != res.Remaining || after["unjudged/undecided"] > before["unjudged/undecided"] || after["unjudged/model_unavailable"] > before["unjudged/model_unavailable"] {
		t.Fatalf("remaining backlog %v (before %v)", after, before)
	}
}
