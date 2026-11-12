package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// ACT-RST-02..06、08..10、12、13：升级后的合成账本上，先预览再放回旧级联忽略的事件。
// 升级本身不放回任何事件；预览只读（审计事件数不变、没有模型请求）；错误参数与授权边界被拒；
// 按来源放回后每条一条 inbound.disposed（操作者是 delegate）；对照组与窗口外的仍是 ignored；再放回 0 条；
// 健康视图的 restored 等于放回数；放开重判走新的级联后，这些事件带判断记录，不再可放回、restored 回到 0。
func TestBacklogRestoreOnUpgradedFixture(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	m, err := ledgerfixture.Generate(home, 3)
	if err != nil {
		t.Fatal(err)
	}
	cfg := "listen: 127.0.0.1:" + strconv.Itoa(freePort(t)) + "\nunjudged_rejudge_days: 0\nbacklog_release_per_day: 5\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cls, chat := modelstub.NewCls(), modelstub.New()
	defer cls.Close()
	defer chat.Close()
	cls.SetDefaultPYes(0.01)                                       // 新级联：分类端点低分即忽略，并留下判断记录
	cls.PYes(modelstub.Key{Contains: "Old cascade ignored"}, 0.05) // 放回的排在前面，且仍低于下限
	addr, stop := start(t, home)
	defer stop()
	base := "http://" + addr.String() + "/v1"
	obs, dlg := m.Tokens["observer"], m.Tokens["delegate"]
	win := m.OldCascadeIgnored.Window

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
	words := map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct", "X-APA-Evidence": "本人：把旧级联忽略的放回来"}
	q := "from=" + win.From + "&to=" + win.To
	body := fmt.Sprintf(`{"from":%q,"to":%q}`, win.From, win.To)

	preview := func(extra string) (p struct {
		Matched int
		Sources []struct {
			Source string
			Count  int
		}
	}) {
		t.Helper()
		code, b := do("GET", "/source-events/restorable?"+q+extra, obs, nil, "")
		if code != 200 || json.Unmarshal(b, &p) != nil {
			t.Fatalf("preview: %d %s", code, b)
		}
		return
	}
	// 审计事件：按类型数条数，并收集操作者。
	disposed := func() (n int, byDlg int) {
		t.Helper()
		for after := int64(0); ; {
			code, b := do("GET", "/audit/events?type=inbound.disposed&limit=500&after="+strconv.FormatInt(after, 10), obs, nil, "")
			var page struct {
				Events []struct {
					Seq   int64
					Actor struct{ Principal string }
				}
			}
			if code != 200 || json.Unmarshal(b, &page) != nil {
				t.Fatalf("audit: %d %s", code, b)
			}
			for _, e := range page.Events {
				n++
				if e.Actor.Principal != "kernel:inbound" {
					byDlg++
				}
				after = e.Seq
			}
			if len(page.Events) < 500 {
				return
			}
		}
	}
	restoredInHealth := func() int {
		t.Helper()
		code, b := do("GET", "/health", obs, nil, "")
		var h struct {
			Signals []struct {
				ID     string
				Detail json.RawMessage
			}
		}
		if code != 200 || json.Unmarshal(b, &h) != nil || len(h.Signals) != 13 {
			t.Fatalf("health: %d %s", code, b)
		}
		for _, s := range h.Signals {
			if s.ID == "unjudged_backlog" {
				var d struct{ Restored int }
				if json.Unmarshal(s.Detail, &d) != nil {
					t.Fatalf("detail: %s", s.Detail)
				}
				return d.Restored
			}
		}
		t.Fatal("no unjudged_backlog signal")
		return 0
	}
	// ignoredIDs 是全部来源里仍为 ignored 的事件 id。
	ignoredIDs := func() map[string]bool {
		t.Helper()
		out := map[string]bool{}
		_, b := do("GET", "/sources", obs, nil, "")
		var srcs struct{ Sources []struct{ ID string } }
		_ = json.Unmarshal(b, &srcs)
		for _, s := range srcs.Sources {
			for after := "0"; ; {
				code, b := do("GET", "/sources/"+s.ID+"/events?disposition=ignored&limit=500&after="+after, obs, nil, "")
				var page struct {
					Events    []struct{ ID string }
					NextAfter int64 `json:"next_after"`
				}
				if code != 200 || json.Unmarshal(b, &page) != nil {
					t.Fatalf("events: %d %s", code, b)
				}
				for _, e := range page.Events {
					out[e.ID] = true
				}
				if page.NextAfter == 0 {
					break
				}
				after = strconv.FormatInt(page.NextAfter, 10)
			}
		}
		return out
	}

	// ACT-RST-02：升级本身没有放回任何事件。
	if restoredInHealth() != 0 {
		t.Fatal("the upgrade restored events")
	}
	baseDisposed, baseCaller := disposed() // 升级前已有的非内核操作者（规则、本人的忽略）不算

	// ACT-RST-03：预览只读，条数与分布同清单。
	p := preview("")
	if p.Matched != m.OldCascadeIgnored.Count || len(p.Sources) != len(m.OldCascadeIgnored.BySource) {
		t.Fatalf("preview %+v, manifest %+v", p, m.OldCascadeIgnored)
	}
	var names []string
	for _, s := range p.Sources {
		if s.Count != m.OldCascadeIgnored.BySource[s.Source] {
			t.Fatalf("source %s: %d, manifest %d", s.Source, s.Count, m.OldCascadeIgnored.BySource[s.Source])
		}
		names = append(names, s.Source)
	}
	sort.Strings(names)
	first := names[0]
	if n, _ := disposed(); n != baseDisposed || len(chat.Requests()) != 0 || len(cls.Batches()) != 0 {
		t.Fatal("preview wrote or called a model")
	}

	// ACT-RST-04：按来源限定；错误参数被拒。
	if p := preview("&source=" + first); p.Matched != m.OldCascadeIgnored.BySource[first] {
		t.Fatalf("source preview: %+v", p)
	}
	for _, c := range []struct {
		path string
		want int
	}{{"from=" + win.From + "&to=" + win.From, 400}, {"from=" + win.To + "&to=" + win.From, 400}, {"to=" + win.To, 400},
		{q + "&source=no-such-source", 404}} {
		if code, b := do("GET", "/source-events/restorable?"+c.path, obs, nil, ""); code != c.want {
			t.Fatalf("%s: %d %s", c.path, code, b)
		}
	}

	// ACT-RST-05：缺原话、observer 都不能放回，预览仍是全部。
	if code, _ := do("POST", "/source-events/restore", dlg, map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct"}, body); code != 403 {
		t.Fatalf("without words: %d", code)
	}
	if code, _ := do("POST", "/source-events/restore", dlg, map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "group", "X-APA-Evidence": "本人：放回"}, body); code != 403 {
		t.Fatalf("group session: %d", code)
	}
	if code, _ := do("POST", "/source-events/restore", obs, words, body); code != 403 {
		t.Fatalf("observer: %d", code)
	}
	if p := preview(""); p.Matched != m.OldCascadeIgnored.Count {
		t.Fatalf("a refused call restored events: %+v", p)
	}

	// ACT-RST-06：按来源放回；每条一条 inbound.disposed，操作者是 delegate；没有模型请求。
	code, b := do("POST", "/source-events/restore", dlg, words, fmt.Sprintf(`{"from":%q,"to":%q,"source":%q}`, win.From, win.To, first))
	var res struct{ Restored int }
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Restored != m.OldCascadeIgnored.BySource[first] {
		t.Fatalf("restore %s: %d %s", first, code, b)
	}
	if n, byDlg := disposed(); n != baseDisposed+res.Restored || byDlg != baseCaller+res.Restored || len(chat.Requests()) != 0 || len(cls.Batches()) != 0 {
		t.Fatalf("disposed %d (by the caller %d), restored %d", n, byDlg, res.Restored)
	}

	// 其余来源，再 ACT-RST-09：同一时间段再放回 0 条（含同一幂等键两次）。
	if code, b = do("POST", "/source-events/restore", dlg, words, body); code != 200 || json.Unmarshal(b, &res) != nil || res.Restored != m.OldCascadeIgnored.Count-m.OldCascadeIgnored.BySource[first] {
		t.Fatalf("restore rest: %d %s", code, b)
	}
	after, _ := disposed()
	for _, key := range []string{"", "k1", "k1"} {
		h := map[string]string{"Idempotency-Key": key}
		for k, v := range words {
			h[k] = v
		}
		if code, b = do("POST", "/source-events/restore", dlg, h, body); code != 200 || json.Unmarshal(b, &res) != nil || res.Restored != 0 {
			t.Fatalf("again (%q): %d %s", key, code, b)
		}
	}
	if n, _ := disposed(); n != after {
		t.Fatal("a repeated restore wrote events")
	}

	// ACT-RST-08：对照组与窗口外的仍是 ignored；旧级联的都不再是。
	still := ignoredIDs()
	for _, group := range [][]string{m.RuleIgnored, m.UserIgnored, m.OutsideWindow} {
		for _, id := range group {
			if !still[id] {
				t.Fatalf("%s must stay ignored", id)
			}
		}
	}
	if len(still) != len(m.RuleIgnored)+len(m.UserIgnored)+len(m.OutsideWindow) {
		t.Fatalf("%d events still ignored, want only the control groups", len(still))
	}
	// ACT-RST-10：健康视图计入 restored。
	if got := restoredInHealth(); got != m.OldCascadeIgnored.Count {
		t.Fatalf("health restored %d, want %d", got, m.OldCascadeIgnored.Count)
	}

	// ACT-RST-12、13：本人配上端点，restored 的事件只经放开重判；新级联忽略后带判断记录，不再可放回。
	lg, _ := json.Marshal(map[string]string{"password": m.WebSecret})
	resp, err := http.Post(base+"/auth/login", "application/json", bytes.NewReader(lg))
	if err != nil {
		t.Fatal(err)
	}
	var li struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&li)
	resp.Body.Close()
	ep := fmt.Sprintf(`{"judge_endpoint":{"url":%q,"protocol":"probability-judgment/1","locality":"local"},"generate_endpoint":{"url":%q,"model":"acc-model","api_key":"k","locality":"local"}}`, cls.URL(), chat.URL())
	req, _ := http.NewRequest("PUT", base+"/settings/model-endpoints", strings.NewReader(ep))
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	req.Header.Set("X-CSRF-Token", li.CSRF)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("set endpoints: %v %v", r, err)
	}
	for _, step := range []string{"rules", "score"} { // 积压放开的第 1、2 步：放回的事件同样适用
		if code, b = do("POST", "/source-events/"+step, dlg, words, `{}`); code != 200 {
			t.Fatalf("%s: %d %s", step, code, b)
		}
	}
	cls.Reset()
	cls.SetDefaultPYes(0.01)
	cls.PYes(modelstub.Key{Contains: "Old cascade ignored"}, 0.05)
	code, b = do("POST", "/source-events/rejudge", dlg, words, `{}`)
	if code != 200 {
		t.Fatalf("rejudge: %d %s", code, b)
	}
	if len(cls.Batches()) == 0 {
		t.Fatal("the screening question was not asked")
	}
	if p := preview(""); p.Matched != 0 {
		t.Fatalf("an event that went through the new cascade is restorable again: %+v", p)
	}
	if got := restoredInHealth(); got != 0 {
		t.Fatalf("health restored %d after rejudge, want 0", got)
	}
}
