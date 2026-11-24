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

// apa merge：缺省与 --dry-run 只演练（副本上、服务开着也能跑、不动账本）；端口被别的程序占着、又没有 serve 的主机入口应答时 --apply 拒绝；
// 参数不合法拒绝；没有服务时离线跑，补打与合并都能重复执行（账本层的幂等、护栏与对照见 inbound 的测试）。
func TestMergeDryRunNeedsNoStopAndApplyRefusesWithoutHostEntry(t *testing.T) {
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
		if r := apa(t, home, "", "merge", sub, "--apply"); r.code != 6 || !strings.Contains(r.errOut, "does not answer") {
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
	for range 2 { // 没有服务：离线跑
		if r := apa(t, home, "", "merge", "mark-pending", "--apply", "--by", "tester"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) || !strings.Contains(r.out, `"marked":[]`) {
			t.Fatalf("mark-pending --apply: %+v", r)
		}
		if r := apa(t, home, "", "merge", "run", "--apply"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) || !strings.Contains(r.out, `"matched":0`) {
			t.Fatalf("run --apply: %+v", r)
		}
	}
}

// apa serve 在跑时 --apply 不用停服务：经数据目录里的主机本地入口交给服务进程执行（端口被 serve 占着，离线跑不可能成功），
// 连跑两遍都成功；套接字只在 0700 的目录里；服务停了套接字随之消失，再跑走离线。
func TestMergeApplyRunsInsideServe(t *testing.T) {
	root, err := os.MkdirTemp("", "apa") // Unix 套接字路径有长度上限，测试目录要短
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	home := filepath.Join(root, "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen: 127.0.0.1:"+strconv.Itoa(freePort(t))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stop := start(t, home)
	if st, err := os.Stat(filepath.Join(home, "host")); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("host entry dir must be 0700: %v %v", st, err)
	}
	for range 2 {
		if r := apa(t, home, "", "merge", "mark-pending", "--apply", "--by", "tester"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) || !strings.Contains(r.out, `"by":"tester"`) {
			t.Fatalf("mark-pending --apply while serving: %+v", r)
		}
		if r := apa(t, home, "", "merge", "run", "--apply"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) || !strings.Contains(r.out, `"matched":0`) {
			t.Fatalf("run --apply while serving: %+v", r)
		}
	}
	stop()
	if _, err := os.Stat(filepath.Join(home, "host", "serve.sock")); !os.IsNotExist(err) {
		t.Fatalf("the socket goes away with the service: %v", err)
	}
	if r := apa(t, home, "", "merge", "run", "--apply"); r.code != 0 || !strings.Contains(r.out, `"dry_run":false`) {
		t.Fatalf("offline after stop: %+v", r)
	}
}
