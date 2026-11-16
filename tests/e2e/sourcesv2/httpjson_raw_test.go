//go:build e2e

package sourcesv2

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildHTTPJSON 把随发行附带的 http-json 构建进本包的程序目录（helpers_test.go 的程序表不含它）。
func buildHTTPJSON(t *testing.T) {
	t.Helper()
	dir := programs(t)
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "http-json"), "./sources/httpjson/cmd/http-json")
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build http-json: %v\n%s", err, out)
	}
}

// e2e-source-raw（http-json 部分）：ACT-OAD-04 被映射的那一条 JSON 原样进 original，字段仍按映射取。
func TestFlowOtherAdaptersRawHTTPJSON(t *testing.T) {
	k := setup(t)
	buildHTTPJSON(t)
	// 键序、空白、数字写法与未映射字段都要原样保留；录制样本内联，不连外部。
	const row1 = `{"uid":"h1","kind":"ticket","headline":"工单 1","at":"2030-01-01T00:00:00Z","extra":{"n":1.50,"tags":["a",  "b"]},"big":12345678901234567890}`
	const row2 = `{ "uid" : "h2", "kind":"ticket","headline":"工单 2","at":"2030-01-02T00:00:00Z" }`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"items":[`+row1+`,`+row2+`]}}`)
	}))
	defer srv.Close()
	k.addPull("src_httpjson_raw", "http-json", "ticket", nil, map[string]any{"url": srv.URL, "items_path": "data.items",
		"mapping": map[string]any{"id": "uid", "type": "kind", "subject": "headline", "occurred_at": "at"}})

	for id, want := range map[string]string{"h1": row1, "h2": row2} {
		ev := k.settled("src_httpjson_raw", id)
		r := k.fullEvent(ev)
		if r.Original.MediaType != "application/json" || r.Original.Text != want {
			t.Errorf("%s 的 original 应是被映射的那一条 JSON 原样: %q %q", id, r.Original.MediaType, r.Original.Text)
		}
		if ev.Subject == "" || ev.Type != "ticket" {
			t.Errorf("字段仍按映射取: %+v", ev)
		}
		if r.Content.State == "partial" {
			t.Errorf("http-json 不自报 partial: %q", r.Content.State)
		}
	}
	if n := len(k.events("src_httpjson_raw")); n != 2 {
		t.Errorf("事件 id 仍取映射的 id，两条各一次: %d", n)
	}
}
