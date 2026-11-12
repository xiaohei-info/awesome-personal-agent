package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

// ACT-REL-08：GET /version 不带凭据即 200，内容含附录 B.5 的全部字段，且与 CLI 共用的 Current 逐字相同。
func TestGetVersionNeedsNoCredentialAndMatchesCurrent(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, clock.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := core.New(core.Deps{Store: st, Auth: auth.New(nil), Groups: []core.RouteGroup{Routes(7)}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/version", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 without credentials, got %d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	b, _ := json.Marshal(Current(7))
	_ = json.Unmarshal(b, &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("route and Current differ:\n%v\n%v", got, want)
	}
	if got["version"] != "0.2.0" || got["commit"] == "" || got["test_clock"] != clock.TestClockBuild {
		t.Fatalf("bad identity fields: %v", got)
	}
	if got["schema"].(map[string]any)["ledger"] != float64(7) {
		t.Fatalf("schema.ledger must be the value handed in: %v", got["schema"])
	}
	wantProto := map[string]any{"http_api": "v1", "source_contract": float64(1), "channel_ext": float64(1), "probability_judgment": "probability-judgment/1"}
	if !reflect.DeepEqual(got["protocols"], wantProto) {
		t.Fatalf("protocols: %v", got["protocols"])
	}
}
