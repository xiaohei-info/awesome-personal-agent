package items

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 导入（POST /import、GET /imports） ----

// credential 签发一个凭据，返回明文与凭据记录（授权单按凭据 id 签给 delegate）。
func (e *env) credential(kind auth.Kind) (string, auth.Credential) {
	e.t.Helper()
	var s string
	var c auth.Credential
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { c, s, err = e.auth.Issue(tx, auth.Spec{Kind: kind}); return }); err != nil {
		e.t.Fatal(err)
	}
	return s, c
}

func ndjson(r *http.Request) { r.Header.Set("Content-Type", ImportContentType) }

func impRec(ext string) string {
	return `{"type":"item","external_id":"` + ext + `","item":{"kind":"task","title":"t"},"history":[{"at":"2025-10-10T09:00:00+08:00","event":"created","phase":"open"}]}`
}

func impRecs(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(impRec(fmt.Sprintf("R-%04d", i)) + "\n")
	}
	return b.String()
}

type importResp struct {
	Batch   string `json:"batch"`
	Results []struct {
		Line    int    `json:"line"`
		Outcome string `json:"outcome"`
		Object  string `json:"object"`
	} `json:"results"`
	Created, Existing, Invalid int
}

func decodeImport(t *testing.T, rec *httptest.ResponseRecorder, want int) importResp {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d (want %d): %s", rec.Code, want, rec.Body)
	}
	var r importResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// ACT-IMP-02/03：本人在 Web 登录下导入不需要授权单；预检没有 batch，不写账本；GET /imports 列出批次。
func TestWebImportAndDryRun(t *testing.T) {
	e := setup(t)
	headOf := func() (h int64) {
		_ = e.st.View(bg, func(tx *store.Tx) (err error) { h, err = eventlog.Head(tx); return })
		return
	}
	before := headOf()
	dry := decodeImport(t, e.do("POST", "/v1/import?source=acc-imp&dry_run=1", impRecs(2), both(e.web, ndjson)), 200)
	if dry.Batch != "" || dry.Created != 2 || len(dry.Results) != 2 || dry.Results[1].Line != 2 {
		t.Fatalf("dry run: %+v", dry)
	}
	if n := headOf() - before; n != 0 {
		t.Fatalf("dry run wrote %d events", n)
	}
	if got := e.do("GET", "/v1/imports", "", e.web).Body.String(); !strings.Contains(got, `"batches":[]`) {
		t.Fatalf("no batch after a dry run: %s", got)
	}
	r := decodeImport(t, e.do("POST", "/v1/import?source=acc-imp", impRecs(2)+"\n", both(e.web, ndjson)), 200)
	if !strings.HasPrefix(r.Batch, "imp_") || r.Created != 2 || r.Results[0].Object == "" {
		t.Fatalf("import: %+v", r)
	}
	var list struct {
		Batches []struct {
			ID, Source, Actor string
			Created           int
			StartedAt         time.Time `json:"started_at"`
			Grant             string
		}
	}
	if err := json.Unmarshal(e.do("GET", "/v1/imports", "", e.web).Body.Bytes(), &list); err != nil || len(list.Batches) != 1 {
		t.Fatalf("%v %+v", err, list)
	}
	if b := list.Batches[0]; b.ID != r.Batch || b.Source != "acc-imp" || b.Actor != "owner" || b.Created != 2 || b.Grant != "" || b.StartedAt.IsZero() {
		t.Fatalf("batch: %+v", b)
	}
}

// ACT-IMP-08：超过 500 行或 8 MiB 返回 413；带 batch 的第二次请求归入同一批次，来源名不同被拒。
func TestLimitsAndBatchReuse(t *testing.T) {
	e := setup(t)
	if rec := e.do("POST", "/v1/import?source=acc-imp", impRecs(501), both(e.web, ndjson)); rec.Code != 413 {
		t.Fatalf("501 lines: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/import?source=acc-imp", strings.Repeat("x", 9<<20), both(e.web, ndjson)); rec.Code != 413 {
		t.Fatalf("9 MiB: %d", rec.Code)
	}
	first := decodeImport(t, e.do("POST", "/v1/import?source=acc-imp", impRecs(500), both(e.web, ndjson)), 200)
	rest := strings.ReplaceAll(impRecs(100), "R-", "S-")
	second := decodeImport(t, e.do("POST", "/v1/import?source=acc-imp&batch="+first.Batch, rest, both(e.web, ndjson)), 200)
	if second.Batch != first.Batch || second.Created != 100 {
		t.Fatalf("second: %+v", second)
	}
	var list struct{ Batches []struct{ Created int } }
	_ = json.Unmarshal(e.do("GET", "/v1/imports", "", e.web).Body.Bytes(), &list)
	if len(list.Batches) != 1 || list.Batches[0].Created != 600 {
		t.Fatalf("one batch of 600: %+v", list)
	}
	if rec := e.do("POST", "/v1/import?source=other&batch="+first.Batch, impRecs(1), both(e.web, ndjson)); rec.Code != 400 {
		t.Fatalf("batch of another source: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"Acc_Imp", strings.Repeat("a", 33), ""} {
		if rec := e.do("POST", "/v1/import?source="+bad, impRecs(1), both(e.web, ndjson)); rec.Code != 400 {
			t.Fatalf("source %q: %d", bad, rec.Code)
		}
	}
	if rec := e.do("POST", "/v1/import?source=acc-imp", impRecs(1), e.web); rec.Code != 400 {
		t.Fatalf("wrong content type: %d", rec.Code)
	}
}

// ACT-IGR-02/03/04：delegate 只凭授权单；不带、只带 X-APA-Evidence、来源名不符、超限都是 403（退出码 23）；观察者 403。
func TestDelegateGrantOverHTTP(t *testing.T) {
	e := setup(t)
	secret, cred := e.credential(auth.Delegate)
	var g policy.Grant
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		g, err = policy.IssueGrant(tx, e.auth, importOwner, time.Now(), policy.GrantInput{Grantee: policy.Grantee{Credential: cred.ID}, Scope: policy.Scope{All: true},
			Actions: []string{policy.ImportApply}, Import: &policy.ImportScope{Source: "acc-imp3", MaxRecords: 3}, ExpiresAt: time.Now().Add(time.Hour)})
		return
	}); err != nil {
		t.Fatal(err)
	}
	bearer := func(extra ...string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
			r.Header.Set("Content-Type", ImportContentType)
			for i := 0; i < len(extra); i += 2 {
				r.Header.Set(extra[i], extra[i+1])
			}
		}
	}
	codeOf := func(rec *httptest.ResponseRecorder, status int) string {
		t.Helper()
		var b struct{ Code string }
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		if rec.Code != status {
			t.Fatalf("status %d (want %d): %s", rec.Code, status, rec.Body)
		}
		return b.Code
	}
	if c := codeOf(e.do("POST", "/v1/import?source=acc-imp3", impRecs(2), bearer()), 403); c != "grant_required" {
		t.Fatalf("no grant: %s", c)
	}
	if c := codeOf(e.do("POST", "/v1/import?source=acc-imp3", impRecs(2), bearer("X-APA-Evidence", "我说导入")), 403); c != "grant_required" {
		t.Fatalf("words only: %s", c)
	}
	if c := codeOf(e.do("POST", "/v1/import?source=acc-other", impRecs(1), bearer("X-APA-Grant", g.ID)), 403); c != "import_source_mismatch" {
		t.Fatalf("source: %s", c)
	}
	r := decodeImport(t, e.do("POST", "/v1/import?source=acc-imp3", impRecs(2), bearer("X-APA-Grant", g.ID)), 200)
	if r.Created != 2 {
		t.Fatalf("%+v", r)
	}
	if c := codeOf(e.do("POST", "/v1/import?source=acc-imp3", strings.ReplaceAll(impRecs(2), "R-", "Q-"), bearer("X-APA-Grant", g.ID)), 403); c != "import_limit_exceeded" {
		t.Fatalf("limit: %s", c)
	}
	var list struct{ Batches []struct{ Grant string } }
	_ = json.Unmarshal(e.do("GET", "/v1/imports", "", bearer()).Body.Bytes(), &list)
	if len(list.Batches) != 1 || list.Batches[0].Grant != g.ID {
		t.Fatalf("batch grant: %+v", list)
	}
	obs, _ := e.credential(auth.Observer)
	rec := e.do("POST", "/v1/import?source=acc-imp3", impRecs(1), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+obs)
		r.Header.Set("Content-Type", ImportContentType)
		r.Header.Set("X-APA-Grant", g.ID)
	})
	if codeOf(rec, 403) != "permission_denied" {
		t.Fatal("an observer cannot import")
	}
}

var importOwner = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
