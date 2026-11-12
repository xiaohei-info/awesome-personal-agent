package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apijudgments "github.com/xiaohei-info/awesome-personal-agent/api/judgments"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/judgment" // 登记 judgment 子命令
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type roundTrip struct{ h http.Handler }

func (rt roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.RemoteAddr = "127.0.0.1:5000"
	rt.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// ACT-JRC-02、ACT-JRC-05：在完整账本结构上，每条判断记录逐级可读；运行凭据读目标事项的记录 200，
// 读别的事项的记录 404，CLI 退出码 5；记录里没有资料正文。
func TestRunCredentialReadsOnlyTargetItemJudgmentRecords(t *testing.T) {
	ctx := context.Background()
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	x := command.New(s, func() time.Time { return now })
	if err := kitems.Register(x, kitems.NewKinds()); err != nil {
		t.Fatal(err)
	}
	mk := func(title string) string {
		res, err := x.Do(ctx, command.Request{Identity: command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb},
			Command: "item.create", Input: json.RawMessage(`{"kind":"task","title":"` + title + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var it struct{ ID string }
		_ = json.Unmarshal(res.Value, &it)
		return it.ID
	}
	target, other := mk("target"), mk("other")
	p := func(v float64) *float64 { return &v }
	yes := true
	var runSecret string
	if err := s.Update(ctx, func(tx *store.Tx) error {
		for _, id := range []string{"JRC-HIGH", "JRC-MID"} {
			if err := runner.AppendInternal(tx, domain.Source{ID: "src_jrc"}, domain.SourceEvent{ID: id, Type: "n", Subject: id, Summary: "s", Detail: "JRC-BODY", Link: "https://x.example/" + id, OccurredAt: now}); err != nil {
				return err
			}
		}
		evs, err := runner.Events(tx, "src_jrc")
		if err != nil {
			return err
		}
		for i, to := range []string{target, other} {
			if err := runner.SetDisposition(tx, evs[i].ID, domain.DispositionProposed, to, ""); err != nil {
				return err
			}
		}
		band := record.Band{Low: 0.15, High: 0.60}
		high := record.Record{ID: "jd_high", Use: "inbound_screen", Template: "triage/triage-screen@abc", Subject: evs[0].ID, Source: "src_jrc", Fingerprint: "f1", Band: band,
			DecidedBy: record.DecidedClassifier, Answer: "yes", CreatedAt: now,
			Levels: []record.Level{{Backend: record.BackendClassifier, Endpoint: "http://127.0.0.1:1", Locality: "local", Model: "cls", PYes: p(0.90), ProbabilitySource: "native", Outcome: record.OutcomeDecided, DurationMs: 3}}}
		mid := record.Record{ID: "jd_mid", Use: "inbound_screen", Template: "triage/triage-screen@abc", Subject: evs[1].ID, Source: "src_jrc", Fingerprint: "f2", Band: band,
			DecidedBy: record.DecidedChat, Answer: "yes", CreatedAt: now.Add(time.Second),
			Levels: []record.Level{
				{Backend: record.BackendClassifier, Model: "cls", PYes: p(0.30), Outcome: record.OutcomeUncertain, DurationMs: 3},
				{Backend: record.BackendChat, Model: "chat", Answer: "yes", Sufficient: &yes, Outcome: record.OutcomeDecided, DurationMs: 9}}}
		for _, r := range []record.Record{high, mid} {
			if err := record.Put(tx, r); err != nil {
				return err
			}
		}
		run, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunTask, Target: target, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, now)
		if err != nil {
			return err
		}
		runSecret, err = runidentity.Issue(tx, run.ID, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	au := auth.New(nil)
	var obs string
	if err := s.Update(ctx, func(tx *store.Tx) (err error) { _, obs, err = au.Issue(tx, auth.Spec{Kind: auth.Observer}); return }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: s, Auth: au, Groups: []core.RouteGroup{apijudgments.Routes(s, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	get := func(secret, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	var list struct{ Judgments []record.Record }
	r := get(obs, "/v1/judgments?source=src_jrc&use=inbound_screen")
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &list) != nil || len(list.Judgments) != 2 {
		t.Fatalf("observer list: %d %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "JRC-BODY") {
		t.Fatal("judgment records must not contain source content")
	}
	if g := list.Judgments[1]; g.DecidedBy != "chat" || len(g.Levels) != 2 || g.Levels[0].Outcome != "uncertain" || g.Levels[1].Outcome != "decided" {
		t.Fatalf("JRC-MID levels: %+v", g)
	}

	if r := get(runSecret, "/v1/judgments/jd_high"); r.Code != 200 {
		t.Fatalf("run credential, target item's record: %d %s", r.Code, r.Body)
	}
	if r := get(runSecret, "/v1/judgments/jd_mid"); r.Code != 404 {
		t.Fatalf("run credential, other item's record: %d %s", r.Code, r.Body)
	}

	apa := func(token string, args ...string) (int, string) {
		var out, errb bytes.Buffer
		vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": token, "APA_HOME": t.TempDir()}
		code := client.Main(args, client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: t.TempDir(), Stdin: strings.NewReader(""),
			Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: roundTrip{srv.Handler()}}})
		return code, out.String()
	}
	if code, out := apa(runSecret, "judgment", "show", "jd_high"); code != 0 || !strings.Contains(out, `"jd_high"`) {
		t.Fatalf("apa judgment show (target): %d %s", code, out)
	}
	if code, _ := apa(runSecret, "judgment", "show", "jd_mid"); code != domain.ExitNotFound {
		t.Fatalf("apa judgment show (other item) exit = %d, want %d", code, domain.ExitNotFound)
	}
}
