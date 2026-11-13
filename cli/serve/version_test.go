package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
)

// ACT-REL-08/09：`apa version` 不需要凭据与内核，输出与运行中内核的 GET /v1/version 相同；
// schema.ledger 等于登记的全部迁移里最高的账本结构版本且不小于 3。
func TestApaVersionMatchesGetVersionAndLatestLedgerMigration(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	cli := apa(t, t.TempDir(), "", "version") // 另一个空目录：不依赖数据目录、凭据或内核
	if cli.code != 0 {
		t.Fatalf("apa version: %+v", cli)
	}
	var fromCLI map[string]any
	if err := json.Unmarshal([]byte(cli.out), &fromCLI); err != nil {
		t.Fatal(err)
	}
	useFreePort(t, home)
	addr, stop := start(t, home)
	defer stop()
	resp, err := http.Get("http://" + addr.String() + "/v1/version") // 不带 Authorization
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /v1/version: %d %s", resp.StatusCode, body)
	}
	var fromHTTP map[string]any
	if err := json.Unmarshal(body, &fromHTTP); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromCLI, fromHTTP) {
		t.Fatalf("CLI and HTTP differ:\n%v\n%v", fromCLI, fromHTTP)
	}
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	ledger := fromHTTP["schema"].(map[string]any)["ledger"].(float64)
	if int(ledger) != reg.SchemaLedger() || ledger < 3 {
		t.Fatalf("schema.ledger=%v, latest migration=%d", ledger, reg.SchemaLedger())
	}
}

func TestApaVersionRejectsArguments(t *testing.T) {
	if r := apa(t, t.TempDir(), "", "version", "extra"); r.code != 2 {
		t.Fatalf("want exit 2: %+v", r)
	}
}
