package judgments

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/spotcheck"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type noDraft struct{}

func (noDraft) DraftProposal(context.Context, command.Identity, string) (string, error) {
	return "", nil
}

func (e *env) withSpotCheck() {
	e.t.Helper()
	x := command.New(e.st, func() time.Time { return e.now })
	if err := spotcheck.Register(x); err != nil {
		e.t.Fatal(err)
	}
	var err error
	e.srv, err = core.New(core.Deps{Store: e.st, Auth: e.au, Groups: []core.RouteGroup{
		Routes(e.st, &spotcheck.Service{Store: e.st, Exec: x, Proposer: noDraft{}, Clock: func() time.Time { return e.now }})}})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) post(secret, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ACT-JRC-17：CLI 与页面读的是同一个 API；池里只有被初筛忽略的事件，运行凭据看不到，观察者不能填结论。
func TestSpotCheckPoolIsReadOnlyForObservers(t *testing.T) {
	e := setup(t)
	e.withSpotCheck()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, domain.Source{ID: "src_1"}, domain.SourceEvent{ID: "e_ign", Type: "n", Subject: "noise", Summary: "s", OccurredAt: e.now}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, "src_1")
		if err != nil {
			return err
		}
		id := evs[len(evs)-1].ID
		if err := runner.SetDisposition(tx, id, domain.DispositionIgnored, "", ""); err != nil {
			return err
		}
		r := rec("jd_ign", id, e.now)
		r.Answer, r.Levels[0].PYes = "no", f64(0.05)
		return record.Put(tx, r)
	}); err != nil {
		t.Fatal(err)
	}
	obs := e.token(auth.Observer)
	r := e.get(obs, "/v1/judgments/spot-check")
	var got struct {
		SpotCheck []spotcheck.Entry `json:"spot_check"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &got)
	if r.Code != 200 || len(got.SpotCheck) != 1 || got.SpotCheck[0].Judgment != "jd_ign" || got.SpotCheck[0].Subject != "noise" {
		t.Fatalf("pool: %d %s", r.Code, r.Body)
	}
	if r = e.get(e.run, "/v1/judgments/spot-check"); r.Code != 403 {
		t.Fatalf("run credential: %d %s", r.Code, r.Body)
	}
	if r = e.get(obs, "/v1/judgments/spot-check?limit=0"); r.Code != 400 {
		t.Fatalf("limit=0: %d", r.Code)
	}
	if r = e.post(obs, "/v1/judgments/jd_ign/review", `{"result":"correct"}`); r.Code != 403 {
		t.Fatalf("observer review must be refused: %d %s", r.Code, r.Body)
	}
	if again := e.get(obs, "/v1/judgments/jd_ign"); strings.Contains(again.Body.String(), `"review"`) {
		t.Fatalf("refused review left a verdict: %s", again.Body)
	}
}
