package serve

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// apa repair-merges：服务占着端口时拒绝、不开账本；--dry-run 照做在副本上，服务开着也能跑；真跑与重跑都成功（账本层的幂等见 inbound 的测试）。
func TestRepairMergesRefusesWhileServingAndDryRunNeedsNoStop(t *testing.T) {
	home := initOnFreePort(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busy := "listen: 127.0.0.1:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port) + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(busy), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(home, "ledger.db")
	before, _ := os.Stat(ledger)
	if r := apa(t, home, "", "repair-merges"); r.code != 6 || !strings.Contains(r.errOut, "apa serve is running") {
		t.Fatalf("want conflict exit 6 while the port is taken: %+v", r)
	}
	r := apa(t, home, "", "repair-merges", "--dry-run")
	var out struct {
		OK       bool `json:"ok"`
		DryRun   bool `json:"dry_run"`
		Items    int  `json:"items"`
		Messages int  `json:"messages"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.out), &out) != nil || !out.OK || !out.DryRun || out.Items != 0 {
		t.Fatalf("dry run: %+v", r)
	}
	if after, _ := os.Stat(ledger); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("refusal and dry run must not touch the ledger")
	}
	l.Close()
	for range 2 {
		if r := apa(t, home, "", "repair-merges"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) {
			t.Fatalf("real run: %+v", r)
		}
	}
	if r := apa(t, home, "", "repair-merges", "extra"); r.code == 0 {
		t.Fatalf("unknown argument must be refused: %+v", r)
	}
}
