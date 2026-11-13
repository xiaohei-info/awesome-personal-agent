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

// ACT-BKL-09、ACT-BKL-10：升级后的合成账本上，本人配好两个端点、依次只跑规则、打分，再放开重判：
// 今天从积压新进入建议流的恰为 backlog_release_per_day（5）条，都是 [FX-HIGH]；输出 deferred 等于剩余积压；
// 每个放回的事件各记一条 inbound.requeued（操作者是调用者）；同一天再放开只推迟不放行，积压数不变；缺本人原话被拒。
func TestBacklogRejudgeOnUpgradedFixture(t *testing.T) {
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
	cls.PYes(modelstub.Key{Contains: "[FX-HIGH]"}, 0.90)
	cls.PYes(modelstub.Key{Contains: "[FX-MID]"}, 0.40)
	cls.PYes(modelstub.Key{Contains: "[FX-LOW]"}, 0.05)
	yes := true
	chat.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "[FX-MID]"}, Answer: "no", Sufficient: &yes})
	chat.Judge(modelstub.ChatJudge{Answer: "yes", Sufficient: &yes})
	chat.SetDefault(modelstub.Reply{Content: `{"title":"起草标题","summary":"起草摘要","kind":"task","recommend":"accept","reason":"值得看"}`})
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
	type event struct{ ID, Subject, Disposition string }
	sources := func() (ids []string) {
		_, b := do("GET", "/sources", m.Tokens["observer"], nil, "")
		var srcs struct{ Sources []struct{ ID string } }
		if json.Unmarshal(b, &srcs) != nil {
			t.Fatalf("sources: %s", b)
		}
		for _, s := range srcs.Sources {
			ids = append(ids, s.ID)
		}
		return
	}
	// events 读全部来源里处置在 dispositions 里的事件。
	events := func(dispositions string) (out []event) {
		t.Helper()
		for _, s := range sources() {
			for after := "0"; ; {
				code, b := do("GET", "/sources/"+s+"/events?disposition="+dispositions+"&limit=500&after="+after, m.Tokens["observer"], nil, "")
				var page struct {
					Events    []event
					NextAfter int64 `json:"next_after"`
				}
				if code != 200 || json.Unmarshal(b, &page) != nil {
					t.Fatalf("events %s: %d %s", s, code, b)
				}
				out = append(out, page.Events...)
				if page.NextAfter == 0 {
					break
				}
				after = strconv.FormatInt(page.NextAfter, 10)
			}
		}
		return
	}
	words := map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct", "X-APA-Evidence": "本人：放开"}

	// 本人在 Web 配上两个端点。
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
	for _, step := range []string{"rules", "score"} {
		if code, b := do("POST", "/source-events/"+step, m.Tokens["delegate"], words, `{}`); code != 200 {
			t.Fatalf("%s: %d %s", step, code, b)
		}
	}
	proposedBefore := map[string]bool{}
	for _, e := range events("proposed") {
		proposedBefore[e.ID] = true
	}
	backlogBefore := len(events("unjudged,expired"))
	cls.Reset() // 之后只看放开重判期间的批次
	cls.PYes(modelstub.Key{Contains: "[FX-HIGH]"}, 0.90)
	cls.PYes(modelstub.Key{Contains: "[FX-MID]"}, 0.40)
	cls.PYes(modelstub.Key{Contains: "[FX-LOW]"}, 0.05)

	// 缺本人原话、observer、负的 limit 都被拒，积压不变。
	if code, _ := do("POST", "/source-events/rejudge", m.Tokens["delegate"], map[string]string{"X-APA-Session": "s1", "X-APA-Session-Kind": "direct"}, `{}`); code != 403 {
		t.Fatalf("rejudge without words: %d", code)
	}
	if code, _ := do("POST", "/source-events/rejudge", m.Tokens["observer"], words, `{}`); code != 403 {
		t.Fatalf("observer rejudge: %d", code)
	}
	if code, _ := do("POST", "/source-events/rejudge", m.Tokens["delegate"], words, `{"limit":-1}`); code != 400 {
		t.Fatalf("negative limit: %d", code)
	}
	if n := len(events("unjudged,expired")); n != backlogBefore {
		t.Fatalf("a refused call changed the backlog: %d -> %d", backlogBefore, n)
	}

	rejudge := func(body string) (res struct{ Requeued, Deferred int }) {
		t.Helper()
		code, b := do("POST", "/source-events/rejudge", m.Tokens["delegate"], words, body)
		if code != 200 || json.Unmarshal(b, &res) != nil {
			t.Fatalf("rejudge: %d %s", code, b)
		}
		return
	}
	res := rejudge(`{}`)
	var fresh []event
	for _, e := range events("proposed") {
		if !proposedBefore[e.ID] {
			fresh = append(fresh, e)
		}
	}
	if len(fresh) != 5 {
		t.Fatalf("%d new suggestions, want exactly 5: %+v", len(fresh), fresh)
	}
	for _, e := range fresh {
		if !strings.Contains(e.Subject, "[FX-HIGH]") {
			t.Fatalf("a lower score went first: %+v", e)
		}
	}
	remaining := len(events("unjudged,expired"))
	if res.Deferred != remaining || res.Requeued == 0 || res.Requeued+remaining != backlogBefore {
		t.Fatalf("%+v, backlog %d -> %d", res, backlogBefore, remaining)
	}
	for _, b := range cls.Batches() {
		if b.Questions > 32 {
			t.Fatalf("batch of %d questions", b.Questions)
		}
	}
	// 每个放回的事件各记一条 inbound.requeued，操作者是调用者（审计事件流，按类型过滤，游标读完）。
	requeued, byKernel := 0, 0
	for after := int64(0); ; {
		code, b := do("GET", "/audit/events?type=inbound.requeued&limit=500&after="+strconv.FormatInt(after, 10), m.Tokens["observer"], nil, "")
		var page struct {
			Events []struct {
				Seq   int64
				Actor struct{ Principal string }
			}
		}
		if code != 200 || json.Unmarshal(b, &page) != nil {
			t.Fatalf("audit events: %d %s", code, b)
		}
		for _, e := range page.Events {
			requeued++
			if e.Actor.Principal == "kernel:inbound" {
				byKernel++
			}
			after = e.Seq
		}
		if len(page.Events) < 500 {
			break
		}
	}
	if requeued != res.Requeued || byKernel != 0 {
		t.Fatalf("inbound.requeued events %d (by the kernel %d), want %d by the caller", requeued, byKernel, res.Requeued)
	}

	// 同一天再放开：只推迟，不放行。
	for _, body := range []string{`{}`, `{"limit":3}`} {
		again := rejudge(body)
		if again.Requeued != 0 || again.Deferred != remaining || len(events("unjudged,expired")) != remaining || len(events("proposed")) != len(proposedBefore)+5 {
			t.Fatalf("same day %s: %+v", body, again)
		}
	}
}
