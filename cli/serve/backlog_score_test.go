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

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// ACT-BKL-05、ACT-BKL-06、ACT-BKL-16：升级后的合成账本上，没有判断端点时影子打分被拒（409，替身无请求、没有分数）；
// 配上分类端点后打分：scored 等于积压总数，处置分布不变，分类端点每批不超过 32 题；来源事件列表 sort=score 按
// [FX-HIGH]（0.90）、[FX-MID]（0.40）、[FX-LOW]（0.05）排列并显示分数。
func TestBacklogShadowScoreOnUpgradedFixture(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	m, err := ledgerfixture.Generate(home, 3)
	if err != nil {
		t.Fatal(err)
	}
	cfg := "listen: 127.0.0.1:" + strconv.Itoa(freePort(t)) + "\nunjudged_rejudge_days: 0\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cls := modelstub.NewCls()
	defer cls.Close()
	cls.PYes(modelstub.Key{Contains: "[FX-HIGH]"}, 0.90)
	cls.PYes(modelstub.Key{Contains: "[FX-MID]"}, 0.40)
	cls.PYes(modelstub.Key{Contains: "[FX-LOW]"}, 0.05)
	addr, stop := start(t, home)
	defer stop()
	base := "http://" + addr.String() + "/v1"

	do := func(method, path, token string, hdr map[string]string, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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
	type event struct {
		ID, Subject, Disposition string
		UnjudgedReason           string   `json:"unjudged_reason"`
		Score                    *float64 `json:"score"`
	}
	sources := func() (ids []string) {
		code, b := do("GET", "/sources", m.Tokens["observer"], nil, "")
		var srcs struct{ Sources []struct{ ID string } }
		if code != 200 || json.Unmarshal(b, &srcs) != nil {
			t.Fatalf("sources: %d %s", code, b)
		}
		for _, s := range srcs.Sources {
			ids = append(ids, s.ID)
		}
		return
	}
	// 一个来源的积压，sort 为空或 score；sort=score 时 next_after 是偏移，翻页读完。
	backlog := func(src, sort string) (out []event) {
		t.Helper()
		for after := "0"; ; {
			q := "/sources/" + src + "/events?disposition=unjudged,expired&limit=50&after=" + after
			if sort != "" {
				q += "&sort=" + sort
			}
			code, b := do("GET", q, m.Tokens["observer"], nil, "")
			var page struct {
				Events    []event
				NextAfter int64 `json:"next_after"`
			}
			if code != 200 || json.Unmarshal(b, &page) != nil {
				t.Fatalf("events %s: %d %s", src, code, b)
			}
			out = append(out, page.Events...)
			if page.NextAfter == 0 {
				return
			}
			after = strconv.FormatInt(page.NextAfter, 10)
		}
	}
	distribution := func() string {
		d := map[string]int{}
		for _, s := range sources() {
			for _, e := range backlog(s, "") {
				d[e.Disposition+"/"+e.UnjudgedReason]++
			}
		}
		return fmt.Sprint(d)
	}
	words := map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct", "X-APA-Evidence": "本人：影子打分"}

	// ACT-BKL-16：没有判断端点。
	before := distribution()
	if code, b := do("POST", "/source-events/score", m.Tokens["delegate"], words, `{}`); code != 409 || !strings.Contains(string(b), "judge endpoint") {
		t.Fatalf("score without a judge endpoint: %d %s", code, b)
	}
	if n := len(cls.Batches()); n != 0 {
		t.Fatalf("classifier was called %d times", n)
	}

	// 本人在 Web 配上判断端点（只在 Web 可改）。
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
	ep := fmt.Sprintf(`{"judge_endpoint":{"url":%q,"protocol":"probability-judgment/1","locality":"local"}}`, cls.URL())
	req, _ := http.NewRequest("PUT", base+"/settings/model-endpoints", strings.NewReader(ep))
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	req.Header.Set("X-CSRF-Token", li.CSRF)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("set judge endpoint: %v %v", r, err)
	}

	// ACT-BKL-05：缺本人原话仍被拒；带原话时全部打分。
	if code, _ := do("POST", "/source-events/score", m.Tokens["delegate"], map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct"}, `{}`); code != 403 {
		t.Fatalf("score without words: %d", code)
	}
	code, b := do("POST", "/source-events/score", m.Tokens["delegate"], words, `{}`)
	var res struct{ Scored, Skipped int }
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Scored != m.Backlog.Total || res.Skipped != 0 {
		t.Fatalf("score: %d %s (backlog %d)", code, b, m.Backlog.Total)
	}
	if after := distribution(); after != before {
		t.Fatalf("scoring changed the dispositions:\n before %s\n after  %s", before, after)
	}
	asked := 0
	for _, bt := range cls.Batches() {
		if bt.Questions > 32 {
			t.Fatalf("batch of %d questions", bt.Questions)
		}
		asked += bt.Questions
	}
	if asked != m.Backlog.Total {
		t.Fatalf("classifier saw %d questions, backlog %d", asked, m.Backlog.Total)
	}

	// ACT-BKL-06：每个来源内 HIGH → MID → LOW，每条带分数；不带 sort 时不显示分数、顺序不变。
	want := map[string]float64{"[FX-HIGH]": 0.90, "[FX-MID]": 0.40, "[FX-LOW]": 0.05}
	seen := 0
	for _, s := range sources() {
		plain := backlog(s, "")
		sorted := backlog(s, "score")
		if len(sorted) != len(plain) {
			t.Fatalf("%s: sort changed the number of events: %d vs %d", s, len(sorted), len(plain))
		}
		for i, e := range sorted {
			if plain[i].Score != nil {
				t.Fatalf("%s: unsorted listing shows a score", s)
			}
			var p float64 = -1
			for mk, v := range want {
				if strings.Contains(e.Subject, mk) {
					p = v
				}
			}
			if p < 0 {
				continue // 结构化事件没有分数标记，分数来自默认概率
			}
			if e.Score == nil || *e.Score != p || (i > 0 && sorted[i-1].Score != nil && *sorted[i-1].Score < *e.Score) {
				t.Fatalf("%s #%d %q: score %v, previous %v", s, i, e.Subject, e.Score, sorted[max(i-1, 0)].Score)
			}
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("no marked events were listed")
	}
}
