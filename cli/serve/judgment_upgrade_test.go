package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apijudgments "github.com/xiaohei-info/awesome-personal-agent/api/judgments"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ACT-UPG-07、ACT-JRC-07：v0.1 写下的判断 Event（run.finished，category: judge）升级后原样可回放，
// 不被搬进判断记录（/judgments 空），载荷字节不变。
func TestV01JudgeEventsReplayAfterUpgradeAndStayOutOfJudgmentRecords(t *testing.T) {
	home := t.TempDir()
	old, err := store.Open(home, v01Registry(t))
	if err != nil {
		t.Fatal(err)
	}
	const actor = `{"type":"system","principal":"inbound","via":"rule"}`
	judge := `{"category":"judge","probability":0.82,"confidence":0.9,"template":"inbound-triage"}`
	for i, e := range []struct{ typ, payload string }{
		{"item.created", `{"title":"from mail"}`},
		{"run.finished", judge},
		{"item.stage_changed", `{"to":"accepted"}`},
	} {
		seedExec(t, old, `INSERT INTO eventlog_event (type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, at)
			VALUES (?, 'itemx', ?, '', ?, 0, '', '', '', ?)`, e.typ, actor, e.payload, 1000+i)
	}
	seedExec(t, old, `INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at, correlation_keys, sender, sender_verified, disposition, item, unjudged_reason)
		VALUES ('mail', 'e1', 'se_old', 'mail', 'r', 'mail', 's', '', '', '', 1, '[]', '', 0, 'proposed', 'itemx', '')`)
	before := take(t, old)
	old.Close()

	reg, _ := registry()
	s, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	au := auth.New(nil)
	var del, obs string
	if err := s.Update(context.Background(), func(tx *store.Tx) (err error) {
		if _, del, err = au.Issue(tx, auth.Spec{Kind: auth.Delegate}); err != nil {
			return err
		}
		_, obs, err = au.Issue(tx, auth.Spec{Kind: auth.Observer})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: s, Auth: au, Groups: []core.RouteGroup{apiwhy.Routes(s), apijudgments.Routes(s, nil)}})
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

	var tl struct {
		Steps []struct {
			Type     string
			Payload  json.RawMessage
			Judgment json.RawMessage
		}
	}
	r := get(del, "/v1/why/itemx")
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &tl) != nil || len(tl.Steps) != 3 {
		t.Fatalf("why after upgrade: %d %s", r.Code, r.Body)
	}
	if st := tl.Steps[1]; st.Type != "run.finished" || string(st.Payload) != judge || st.Judgment != nil {
		t.Fatalf("v0.1 judge event must replay as is: %+v", st)
	}
	if r := get(obs, "/v1/why/itemx"); r.Code != 200 || !strings.Contains(r.Body.String(), "run.finished") {
		t.Fatalf("observer why: %d %s", r.Code, r.Body)
	}
	if r := get(obs, "/v1/judgments?subject=se_old"); r.Code != 200 || !strings.Contains(r.Body.String(), `"judgments":[]`) {
		t.Fatalf("old judgment must not appear as a record: %d %s", r.Code, r.Body)
	}
	if after := take(t, s); after.payload[2] != before.payload[2] {
		t.Fatal("v0.1 judge payload was rewritten by the upgrade")
	}
	if n := scalar(t, s, "SELECT count(*) FROM outcall_judgment"); n != int64(0) {
		t.Fatalf("upgrade must not synthesize judgment records, got %v", n)
	}
}
