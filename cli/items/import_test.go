package items_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	kimports "github.com/xiaohei-info/awesome-personal-agent/kernel/imports"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// plan 写一份 n 条事项的计划文件（外部 id 为 prefix-序号），返回路径。
func plan(t *testing.T, n int, prefix string, extra ...string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"type":"item","external_id":"%s-%04d","item":{"kind":"task","title":"t%d"},"history":[{"at":"2025-10-10T09:00:00+08:00","event":"created","phase":"open"}]}`+"\n", prefix, i, i)
	}
	for _, l := range extra {
		b.WriteString(l + "\n")
	}
	p := filepath.Join(t.TempDir(), "plan.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// importGrant 以本人名义给 delegate 签一张导入专用单。
func (k *kernel) importGrant(source string, max int) string {
	k.t.Helper()
	var id string
	if err := k.st.Update(bg, func(tx *store.Tx) error {
		c, err := k.auth.Resolve(tx, k.tokens[auth.Delegate])
		if err != nil {
			return err
		}
		g, err := policy.IssueGrant(tx, k.auth, owner.Actor, k.now, policy.GrantInput{Grantee: policy.Grantee{Credential: c.ID}, Scope: policy.Scope{All: true},
			Actions: []string{policy.ImportApply}, Import: &policy.ImportScope{Source: source, MaxRecords: max}, ExpiresAt: k.now.Add(24 * 3600e9)})
		id = g.ID
		return err
	}); err != nil {
		k.t.Fatal(err)
	}
	return id
}

func (r run) rows(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(r.out), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

// ACT-IGR-07：CLI 按 500 条分批、沿用同一个批次号，逐行输出 created。
func TestImportSplitsInto500AndKeepsTheBatch(t *testing.T) {
	k := newKernel(t)
	g := k.importGrant("acc-imp4", 1200)
	r := k.ok(auth.Delegate, "import", "--file", plan(t, 1100, "M"), "--source", "acc-imp4", "--grant", g)
	rows := r.rows(t)
	if len(rows) != 1100 || rows[0]["outcome"] != "created" || rows[1099]["outcome"] != "created" || rows[1099]["line"].(float64) != 1100 || rows[500]["line"].(float64) != 501 {
		t.Fatalf("rows: %d, first %v last %v", len(rows), rows[0], rows[len(rows)-1])
	}
	var bs []kimports.Batch
	if err := k.st.View(bg, func(tx *store.Tx) (err error) { bs, err = kimports.Batches(tx); return }); err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].Source != "acc-imp4" || bs[0].Created != 1100 || bs[0].Grant != g {
		t.Fatalf("one batch for the three requests expected: %+v", bs)
	}
	// 重放：全部 existing，不占条数。
	r = k.ok(auth.Delegate, "import", "--file", plan(t, 1100, "M"), "--source", "acc-imp4", "--grant", g)
	for _, row := range r.rows(t) {
		if row["outcome"] != "existing" {
			t.Fatalf("replay row: %v", row)
		}
	}
}

// ACT-IGR-02/04/05：不带单或只带原话退出码 23 grant_required；预检不占条数；来源名不符与超限 23。
func TestImportGrantErrorsAndDryRun(t *testing.T) {
	k := newKernel(t)
	p := plan(t, 2, "G")
	for _, args := range [][]string{{"import", "--file", p, "--source", "acc-imp3"}, {"--evidence", "我说导入", "import", "--file", p, "--source", "acc-imp3"}} {
		if r := k.apa(auth.Delegate, args...); r.code != 23 || k.errCode(r) != "grant_required" {
			t.Fatalf("%v: exit %d %s", args, r.code, r.errOut)
		}
	}
	g := k.importGrant("acc-imp3", 3)
	if r := k.apa(auth.Delegate, "import", "--file", p, "--source", "acc-other", "--grant", g); r.code != 23 || k.errCode(r) != "import_source_mismatch" {
		t.Fatalf("source mismatch: exit %d %s", r.code, r.errOut)
	}
	k.ok(auth.Delegate, "import", "--file", p, "--source", "acc-imp3", "--grant", g) // 已用 2
	if r := k.apa(auth.Delegate, "import", "--file", plan(t, 2, "H"), "--source", "acc-imp3", "--grant", g); r.code != 23 || k.errCode(r) != "import_limit_exceeded" {
		t.Fatalf("over the limit: exit %d %s", r.code, r.errOut)
	}
	for i := 0; i < 3; i++ { // 预检三次不占条数
		rows := k.ok(auth.Delegate, "import", "--file", plan(t, 1, "D"), "--source", "acc-imp3", "--grant", g, "--dry-run").rows(t)
		if len(rows) != 1 || rows[0]["outcome"] != "created" {
			t.Fatalf("dry run: %v", rows)
		}
	}
	if rows := k.ok(auth.Delegate, "import", "--file", plan(t, 1, "D"), "--source", "acc-imp3", "--grant", g).rows(t); rows[0]["outcome"] != "created" {
		t.Fatalf("the dry runs must not have used the grant: %v", rows)
	}
	// 观察者不能导入（退出码 4）。
	if r := k.apa(auth.Observer, "import", "--file", p, "--source", "acc-imp3", "--grant", g); r.code != 4 {
		t.Fatalf("observer: exit %d", r.code)
	}
}

// 不合法的行逐行输出 invalid，行号是计划文件里的行号；空行跳过；"-" 读标准输入；缺参数是用法错误。
func TestImportReportsInvalidRowsByFileLine(t *testing.T) {
	k := newKernel(t)
	g := k.importGrant("acc-imp5", 10)
	p := plan(t, 1, "I", "", `{"type":"item","external_id":"bad","item":{"kind":"delegation","title":"x"},"history":[{"at":"2025-10-10T09:00:00+08:00","event":"created","phase":"open"}]}`)
	rows := k.ok(auth.Delegate, "import", "--file", p, "--source", "acc-imp5", "--grant", g).rows(t)
	if len(rows) != 2 || rows[0]["outcome"] != "created" || rows[1]["outcome"] != "invalid" || rows[1]["code"] != "kind_not_allowed" || rows[1]["line"].(float64) != 3 || rows[1]["external_id"] != "bad" {
		t.Fatalf("rows: %v", rows)
	}
	data, _ := os.ReadFile(plan(t, 1, "S"))
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": k.tokens[auth.Delegate], "APA_HOME": t.TempDir()}
	code := client.Main([]string{"import", "--file", "-", "--source", "acc-imp5", "--grant", g}, client.Env{Getenv: func(s string) string { return vars[s] },
		UserHome: t.TempDir(), Stdin: bytes.NewReader(data), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	if code != 0 || !strings.Contains(out.String(), `"outcome":"created"`) {
		t.Fatalf("stdin: %d %s %s", code, out.String(), errb.String())
	}
	for _, args := range [][]string{{"import"}, {"import", "--file", p}, {"import", "--source", "x"}, {"import", "--file", "/nonexistent", "--source", "x"}} {
		if r := k.apa(auth.Delegate, args...); r.code != 2 {
			t.Errorf("%v: exit %d", args, r.code)
		}
	}
}

// ACT-IMP-10：apa item list --origin-source/--origin-ref 把外部 id 换成账本里的事项 id，输出与 HTTP 一致。
func TestItemListByOrigin(t *testing.T) {
	k := newKernel(t)
	g := k.importGrant("acc-imp", 10)
	k.ok(auth.Delegate, "import", "--file", plan(t, 3, "O"), "--source", "acc-imp", "--grant", g)
	ids := func(r run) []string {
		var out []string
		for _, it := range r.json(t)["items"].([]any) {
			out = append(out, it.(map[string]any)["origin"].(map[string]any)["ref"].(string))
		}
		return out
	}
	if got := ids(k.ok(auth.Observer, "item", "list", "--origin-source", "import:acc-imp")); len(got) != 3 {
		t.Fatalf("by source: %v", got)
	}
	if got := ids(k.ok(auth.Observer, "item", "list", "--origin-source", "import:acc-imp", "--origin-ref", "O-0001")); len(got) != 1 || got[0] != "O-0001" {
		t.Fatalf("by source and ref: %v", got)
	}
	if got := ids(k.ok(auth.Observer, "item", "list", "--origin-source", "import:other")); len(got) != 0 {
		t.Fatalf("other source: %v", got)
	}
}
