package judgment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apijudgments "github.com/xiaohei-info/awesome-personal-agent/api/judgments"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

type handlerTransport struct{ h http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.RemoteAddr = "127.0.0.1:5000"
	h.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

type rig struct {
	t         *testing.T
	st        *store.Store
	au        *auth.Auth
	srv       *core.Server
	runSecret string // 目标事项 A 的运行凭据
}

func f64(v float64) *float64 { return &v }

// newRig 装配真实账本与 /v1/judgments 路由，写入 JRC-MID（挂靠事项 A 的来源事件）与 JRC-LOW（挂靠事项 B），
// 并为事项 A 起一次任务型运行。
func newRig(t *testing.T) *rig {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, core.Module, projects.Module, clock.Module,
		items.Module, runner.Module, proc.Module, runidentity.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	x := command.New(st, func() time.Time { return now })
	if err := items.Register(x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	mk := func(title string) string {
		res, err := x.Do(bg, command.Request{Identity: command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb},
			Command: "item.create", Input: json.RawMessage(`{"kind":"task","title":"` + title + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var it struct{ ID string }
		_ = json.Unmarshal(res.Value, &it)
		return it.ID
	}
	a, b := mk("a"), mk("b")
	r := &rig{t: t, st: st, au: auth.New(nil)}
	yes := true
	if err := st.Update(bg, func(tx *store.Tx) error {
		for _, id := range []string{"e_mid", "e_low"} {
			if err := runner.AppendInternal(tx, domain.Source{ID: "src_1"}, domain.SourceEvent{ID: id, Type: "n", Subject: id, Summary: "s", Link: "https://x.example/" + id, OccurredAt: now}); err != nil {
				return err
			}
		}
		evs, err := runner.Events(tx, "src_1")
		if err != nil {
			return err
		}
		if err := runner.SetDisposition(tx, evs[0].ID, domain.DispositionProposed, a, ""); err != nil {
			return err
		}
		if err := runner.SetDisposition(tx, evs[1].ID, domain.DispositionProposed, b, ""); err != nil {
			return err
		}
		mid := record.Record{ID: "jd_mid", Use: "inbound_screen", Template: "triage/triage-screen@abc", Subject: evs[0].ID, Source: "src_1", Fingerprint: "f1",
			Band: record.Band{Low: 0.15, High: 0.60}, DecidedBy: record.DecidedChat, Answer: "yes", CreatedAt: now,
			Levels: []record.Level{
				{Backend: record.BackendClassifier, Model: "cls", PYes: f64(0.30), Outcome: record.OutcomeUncertain, DurationMs: 12},
				{Backend: record.BackendChat, Model: "chat", Answer: "yes", Sufficient: &yes, Outcome: record.OutcomeDecided, DurationMs: 480}}}
		low := record.Record{ID: "jd_low", Use: "inbound_screen", Template: "triage/triage-screen@abc", Subject: evs[1].ID, Source: "src_1", Fingerprint: "f2",
			Band: record.Band{Low: 0.15, High: 0.60}, DecidedBy: record.DecidedClassifier, Answer: "no", CreatedAt: now.Add(time.Second),
			Levels: []record.Level{{Backend: record.BackendClassifier, Model: "cls", PYes: f64(0.05), Outcome: record.OutcomeDecided, DurationMs: 4}}}
		if err := record.Put(tx, mid); err != nil {
			return err
		}
		if err := record.Put(tx, low); err != nil {
			return err
		}
		run, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunTask, Target: a, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, now)
		if err != nil {
			return err
		}
		r.runSecret, err = runidentity.Issue(tx, run.ID, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.srv, err = core.New(core.Deps{Store: st, Auth: r.au, Groups: []core.RouteGroup{apijudgments.Routes(st, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) observer() string {
	var secret string
	if err := r.st.Update(bg, func(tx *store.Tx) (err error) {
		_, secret, err = r.au.Issue(tx, auth.Spec{Kind: auth.Observer})
		return
	}); err != nil {
		r.t.Fatal(err)
	}
	return secret
}

type result struct {
	code        int
	out, errOut string
}

func (r *rig) apa(token string, args ...string) result {
	r.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": token, "APA_HOME": r.t.TempDir()}
	env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: r.t.TempDir(), Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: handlerTransport{r.srv.Handler()}}}
	return result{client.Main(args, env), out.String(), errb.String()}
}

// ACT-JRC-04：judgment show 逐级列出后端、结果与是否定案，JSON 与 --pretty 一致。
func TestShowListsEveryLevelAndWhoDecided(t *testing.T) {
	r := newRig(t)
	obs := r.observer()
	res := r.apa(obs, "judgment", "show", "jd_mid")
	var got record.Record
	if res.code != 0 || json.Unmarshal([]byte(res.out), &got) != nil || got.ID != "jd_mid" || len(got.Levels) != 2 ||
		got.Levels[0].Outcome != "uncertain" || got.Levels[1].Outcome != "decided" || got.DecidedBy != "chat" {
		t.Fatalf("json: %+v", res)
	}
	p := r.apa(obs, "judgment", "show", "jd_mid", "--pretty")
	for _, want := range []string{"jd_mid", "decided by chat → yes", "1. classifier (cls) p=0.30 in the uncertain band, uncertain", "2. chat (chat) answer=yes, decided — decided"} {
		if p.code != 0 || !strings.Contains(p.out, want) {
			t.Fatalf("pretty lacks %q: %+v", want, p)
		}
	}
	if low := r.apa(obs, "judgment", "show", "jd_low", "--pretty"); !strings.Contains(low.out, "p=0.05 ≤ low 0.15 → no") {
		t.Fatalf("low: %s", low.out)
	}
}

// ACT-JRC-05：运行凭据读目标事项的记录成功，读别的事项的记录退出码 5。
func TestRunCredentialCannotShowUnrelatedRecord(t *testing.T) {
	r := newRig(t)
	if res := r.apa(r.runSecret, "judgment", "show", "jd_mid"); res.code != 0 || !strings.Contains(res.out, `"jd_mid"`) {
		t.Fatalf("target item's record: %+v", res)
	}
	if res := r.apa(r.runSecret, "judgment", "show", "jd_low"); res.code != domain.ExitNotFound {
		t.Fatalf("unrelated record must exit %d: %+v", domain.ExitNotFound, res)
	}
}

func TestShowUsageAndMissing(t *testing.T) {
	r := newRig(t)
	obs := r.observer()
	for _, args := range [][]string{{"judgment"}, {"judgment", "bogus"}, {"judgment", "show"}, {"judgment", "show", "a", "b"}, {"judgment", "show", "--pretty"}} {
		if res := r.apa(obs, args...); res.code != domain.ExitUsage || !strings.Contains(res.errOut, "usage: apa judgment show ") {
			t.Fatalf("%v: %+v", args, res)
		}
	}
	if res := r.apa(obs, "judgment", "show", "jd_nope"); res.code != domain.ExitNotFound {
		t.Fatalf("missing: %+v", res)
	}
}

func TestLevelLineDescribesErrorsAndReuse(t *testing.T) {
	yes := false
	got := LevelLine(record.Level{Backend: "chat", Answer: "?", Sufficient: &yes, Outcome: "error", ErrorCode: "timeout", Reused: true, DurationMs: 7}, record.Band{Low: .1, High: .9})
	for _, w := range []string{"answer=? (not sufficient)", "error timeout", "reused", "7ms"} {
		if !strings.Contains(got, w) {
			t.Fatalf("%q lacks %q", got, w)
		}
	}
	if hi := LevelLine(record.Level{Backend: "classifier", PYes: f64(0.9), Outcome: "decided"}, record.Band{Low: .1, High: .6}); !strings.Contains(hi, "≥ high 0.60 → yes") {
		t.Fatal(hi)
	}
}

// ACT-JRC-08：judgment stats 的 JSON 与 HTTP 读取一致，按来源筛选；未知参数是用法错误。
func TestStatsMatchesHTTPAndFilters(t *testing.T) {
	r := newRig(t)
	obs := r.observer()
	res := r.apa(obs, "judgment", "stats", "--use", "inbound_screen", "--source", "src_1")
	var got struct {
		Rows []struct {
			Backend                   string
			Calls, Decided, Uncertain int
		}
		Rates []struct{ Escalation, Undecided float64 }
	}
	if res.code != 0 || json.Unmarshal([]byte(res.out), &got) != nil || len(got.Rows) != 2 || len(got.Rates) != 1 {
		t.Fatalf("%+v", res)
	}
	// jd_mid 的分类端点不确定、对话端点定案；jd_low 的分类端点定案：升级 1/2。
	if got.Rates[0].Escalation != 0.5 || got.Rates[0].Undecided != 0 {
		t.Fatalf("%+v", got.Rates)
	}
	req := httptest.NewRequest("GET", "/v1/judgments/stats?use=inbound_screen&source=src_1", nil)
	req.Header.Set("Authorization", "Bearer "+obs)
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	var a, b any
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	_ = json.Unmarshal([]byte(res.out), &b)
	if !jsonEqual(a, b) {
		t.Fatalf("CLI %s != HTTP %s", res.out, rec.Body)
	}
	if none := r.apa(obs, "judgment", "stats", "--source", "src_none"); none.code != 0 || !strings.Contains(none.out, `"rows":[]`) {
		t.Fatalf("%+v", none)
	}
	for _, args := range [][]string{{"judgment", "stats", "--bogus", "x"}, {"judgment", "stats", "extra"}} {
		if res := r.apa(obs, args...); res.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, res)
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
