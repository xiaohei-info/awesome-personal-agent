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

// apa merge：缺省与 --dry-run 只演练（副本上、服务开着也能跑、不动账本）；--apply 在服务占着端口时拒绝；参数不合法拒绝；
// 补打与合并都能重复执行（账本层的幂等、护栏与对照见 inbound 的测试）。
func TestMergeDryRunNeedsNoStopAndApplyRefusesWhileServing(t *testing.T) {
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
	for _, sub := range []string{"run", "mark-pending"} {
		if r := apa(t, home, "", "merge", sub, "--apply"); r.code != 6 || !strings.Contains(r.errOut, "apa serve is running") {
			t.Fatalf("%s --apply must be refused while the port is taken: %+v", sub, r)
		}
		for _, args := range [][]string{{"merge", sub}, {"merge", sub, "--dry-run", "--source", "agent:vault-*", "--project", "-", "--since", "2026-11-01", "--limit", "5"}} {
			r := apa(t, home, "", args...)
			var out struct {
				DryRun bool `json:"dry_run"`
			}
			if r.code != 0 || json.Unmarshal([]byte(r.out), &out) != nil || !out.DryRun {
				t.Fatalf("%v: %+v", args, r)
			}
		}
	}
	if after, _ := os.Stat(ledger); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("refusal and dry run must not touch the ledger")
	}
	for _, bad := range [][]string{{"merge"}, {"merge", "nope"}, {"merge", "run", "--dry-run", "--apply"}, {"merge", "run", "--by", "x"},
		{"merge", "run", "--since", "yesterday"}, {"merge", "run", "--limit", "-1"}, {"merge", "run", "extra"}} {
		if r := apa(t, home, "", bad...); r.code != 2 {
			t.Fatalf("%v must be a usage error: %+v", bad, r)
		}
	}
	l.Close()
	for range 2 {
		if r := apa(t, home, "", "merge", "mark-pending", "--apply", "--by", "tester"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) || !strings.Contains(r.out, `"marked":[]`) {
			t.Fatalf("mark-pending --apply: %+v", r)
		}
		if r := apa(t, home, "", "merge", "run", "--apply"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) || !strings.Contains(r.out, `"matched":0`) {
			t.Fatalf("run --apply: %+v", r)
		}
	}
}
