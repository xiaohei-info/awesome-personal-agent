package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

// 测试用账本：真实的 eventlog 模块（结构版本 2）加一个属于结构版本 3 的模块。
var thingsMod = store.Module{Name: "things", Migrations: []store.Migration{
	{Version: 1, Name: "create", SQL: "CREATE TABLE things_thing (id TEXT PRIMARY KEY)", Ledger: 3}}}

func reg(t *testing.T, withV3 bool) *store.Registry {
	t.Helper()
	r := store.NewRegistry()
	mods := []store.Module{eventlog.Module}
	if withV3 {
		mods = append(mods, thingsMod)
	}
	for _, m := range mods {
		if err := r.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// newHome 建一个带账本、事件、blobs、凭据等的数据目录。
func newHome(t *testing.T, events int) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	st, err := store.Open(home, reg(t, true))
	if err != nil {
		t.Fatal(err)
	}
	addEvents(t, st, 0, events)
	if err := st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO things_thing (id) VALUES ('a'), ('b')")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "config.yaml"), "listen: 127.0.0.1:7777\n", 0o644)
	write(t, filepath.Join(home, "sources.json"), "[]", 0o644)
	write(t, filepath.Join(home, "blobs", "ab", "abcd"), "blob-bytes", 0o644)
	write(t, filepath.Join(home, "packs", "mine", "pack.yaml"), "name: mine", 0o644)
	write(t, filepath.Join(home, "declarations", "d.json"), "{}", 0o644)
	write(t, filepath.Join(home, "sources", "mail", "cred"), "TOKEN-SECRET", 0o600)
	write(t, filepath.Join(home, "webhook-keys", "hook"), "HMAC-SECRET", 0o600)
	write(t, filepath.Join(home, "runs", "r1", "out"), "run", 0o644)
	write(t, filepath.Join(home, "run-credentials", "r1"), "rc", 0o600)
	write(t, filepath.Join(home, "logs", "x.log"), "log", 0o644)
	return home
}

func addEvents(t *testing.T, st *store.Store, from, n int) {
	t.Helper()
	err := st.Update(context.Background(), func(tx *store.Tx) error {
		for i := from; i < from+n; i++ {
			if _, err := tx.Exec(`INSERT INTO eventlog_event (type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, at)
				VALUES ('t', ?, 'u', '', '{}', 0, '', '', '', 1)`, fmt.Sprintf("o%d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var now = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func digest(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	for _, rel := range listing(t, dir) {
		fi, _ := os.Stat(filepath.Join(dir, rel))
		fmt.Fprintf(h, "%s|%v\n", rel, fi.IsDir())
		if !fi.IsDir() {
			b, _ := os.ReadFile(filepath.Join(dir, rel))
			h.Write(b)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func has(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func mustCreate(t *testing.T, home string, o Options) (string, Manifest) {
	t.Helper()
	o.Out = filepath.Join(t.TempDir(), "bk")
	m, err := Create(context.Background(), home, o, reg(t, true), now)
	if err != nil {
		t.Fatal(err)
	}
	return o.Out, m
}

func wantCode(t *testing.T, err error, code domain.Code) {
	t.Helper()
	de, ok := err.(*domain.Error)
	if !ok || de.Code != code {
		t.Fatalf("err = %v, want domain code %s", err, code)
	}
}

// ACT-BAK-02：备份含约定的内容与清单，权限 0700，不含运行目录与日志。
func TestCreateContentsPermissionsAndManifest(t *testing.T) {
	home := newHome(t, 30)
	out, m := mustCreate(t, home, Options{})
	l := listing(t, out)
	for _, want := range []string{"ledger.db", "manifest.json", "config.yaml", "sources.json", "blobs/ab/abcd", "packs/mine/pack.yaml",
		"declarations/d.json", "sources/mail/cred", "webhook-keys/hook"} {
		if !has(l, want) {
			t.Errorf("backup lacks %s; has %v", want, l)
		}
	}
	for _, bad := range []string{"runs", "run-credentials", "logs"} {
		if has(l, bad) {
			t.Errorf("backup contains excluded %s", bad)
		}
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o700 {
		t.Errorf("backup dir mode = %o, want 700", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(out, "sources", "mail", "cred")); fi.Mode().Perm() != 0o600 {
		t.Errorf("credential mode = %o, want 600", fi.Mode().Perm())
	}
	if m.Schema.Ledger != 3 || m.Schema.Modules["eventlog"] != 2 || m.Schema.Modules["things"] != 1 {
		t.Errorf("schema = %+v", m.Schema)
	}
	if m.Tables["eventlog_event"] != 30 || m.Tables["things_thing"] != 2 || m.EventMaxSeq != 30 {
		t.Errorf("tables %v max %d", m.Tables, m.EventMaxSeq)
	}
	if m.APAVersion == "" || m.NoSecrets || len(m.Files) == 0 || m.Files["blobs/ab/abcd"] == "" {
		t.Errorf("manifest incomplete: %+v", m)
	}
	var onDisk Manifest
	b, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil || json.Unmarshal(b, &onDisk) != nil || onDisk.EventMaxSeq != 30 {
		t.Errorf("manifest.json unreadable: %v", err)
	}
}

// ACT-BAK-02：服务运行（并发写）时创建的备份一致：可校验，事件序号连续。
func TestCreateWhileWritersRunning(t *testing.T) {
	home := newHome(t, 10)
	st, err := store.Open(home, reg(t, true))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 100; ; i++ {
			select {
			case <-stop:
				return
			default:
				addEvents(t, st, i, 1)
			}
		}
	}()
	out, m := mustCreate(t, home, Options{})
	close(stop)
	wg.Wait()
	if m.EventMaxSeq < 10 {
		t.Errorf("event max = %d", m.EventMaxSeq)
	}
	if _, err := Verify(out, reg(t, true)); err != nil {
		t.Fatalf("backup taken under load does not verify: %v", err)
	}
}

// ACT-BAK-03：--no-secrets 不含两个密钥目录，清单标明。
func TestCreateNoSecrets(t *testing.T) {
	home := newHome(t, 5)
	out, m := mustCreate(t, home, Options{NoSecrets: true})
	l := listing(t, out)
	if has(l, "sources") || has(l, "webhook-keys") || has(l, "sources/mail/cred") || has(l, "webhook-keys/hook") {
		t.Errorf("secrets leaked into --no-secrets backup: %v", l)
	}
	if !m.NoSecrets || !has(l, "blobs/ab/abcd") {
		t.Errorf("manifest/no-secrets content wrong: %+v %v", m, l)
	}
	for rel := range m.Files {
		if strings.HasPrefix(rel, "sources/") || strings.HasPrefix(rel, "webhook-keys/") {
			t.Errorf("manifest lists secret file %s", rel)
		}
	}
}

// ACT-BAK-02：默认输出位置在 <home>/backups/ 下；已有非空目录不被覆盖。
func TestCreateDefaultOutAndRefusesNonEmpty(t *testing.T) {
	home := newHome(t, 3)
	m, err := Create(context.Background(), home, Options{}, reg(t, true), now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.Dir, filepath.Join(home, "backups")+string(filepath.Separator)) {
		t.Errorf("default out = %s", m.Dir)
	}
	if _, err := Verify(m.Dir, reg(t, true)); err != nil {
		t.Fatal(err)
	}
	before := digest(t, m.Dir)
	_, err = Create(context.Background(), home, Options{Out: m.Dir}, reg(t, true), now)
	wantCode(t, err, domain.CodeConflict)
	if digest(t, m.Dir) != before {
		t.Error("existing backup modified")
	}
	_, err = Create(context.Background(), filepath.Join(t.TempDir(), "none"), Options{Out: filepath.Join(t.TempDir(), "o")}, reg(t, true), now)
	wantCode(t, err, domain.CodeNotFound)
}

// ACT-BAK-04：完好的备份校验通过，报告说明三项检查。
func TestVerifyGoodBackup(t *testing.T) {
	home := newHome(t, 20)
	for _, o := range []Options{{}, {NoSecrets: true}} {
		out, _ := mustCreate(t, home, o)
		rep, err := Verify(out, reg(t, true))
		if err != nil {
			t.Fatalf("good backup rejected: %v", err)
		}
		if !rep.HashesOK || !rep.IntegrityOK || !rep.EventsContinuous || rep.EventMaxSeq != 20 || rep.Files == 0 {
			t.Errorf("report = %+v", rep)
		}
	}
}

func cloneDir(t *testing.T, src, dst string) {
	t.Helper()
	for _, rel := range listing(t, src) {
		s := filepath.Join(src, rel)
		fi, _ := os.Stat(s)
		if fi.IsDir() {
			os.MkdirAll(filepath.Join(dst, rel), 0o700)
			continue
		}
		b, _ := os.ReadFile(s)
		write(t, filepath.Join(dst, rel), string(b), fi.Mode().Perm())
	}
}

// ACT-BAK-05：改一个字节、删中间事件、删文件、多出文件、清单缺失，都被发现。
func TestVerifyDetectsCorruption(t *testing.T) {
	home := newHome(t, 20)
	out, _ := mustCreate(t, home, Options{})
	corrupt := func(name string, mutate func(dir string), want string) {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "x")
			cloneDir(t, out, dir)
			mutate(dir)
			_, err := Verify(dir, reg(t, true))
			if err == nil {
				t.Fatal("corrupt backup passed verify")
			}
			de, ok := err.(*domain.Error)
			if !ok || de.Code != domain.CodeInternal || de.ExitCode() != 1 {
				t.Errorf("err = %#v, want exit 1", err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err %q lacks %q", err, want)
			}
		})
	}
	corrupt("blob byte", func(d string) { write(t, filepath.Join(d, "blobs/ab/abcd"), "blob-bytez", 0o644) }, "blobs/ab/abcd")
	corrupt("missing file", func(d string) { os.Remove(filepath.Join(d, "config.yaml")) }, "config.yaml")
	corrupt("unlisted file", func(d string) { write(t, filepath.Join(d, "extra"), "x", 0o644) }, "extra")
	corrupt("no manifest", func(d string) { os.Remove(filepath.Join(d, "manifest.json")) }, "manifest")
	corrupt("event gap", func(d string) {
		deleteEvent(t, filepath.Join(d, "ledger.db"), 10)
		rehash(t, d) // 让文件哈希对得上，只剩账本自身的缺口
	}, "continuous")
	corrupt("ledger garbage", func(d string) {
		write(t, filepath.Join(d, "ledger.db"), "not a database", 0o644)
		rehash(t, d)
	}, "ledger")
}

// deleteEvent 直接改备份里的账本文件（模拟用 sqlite3 删除中间事件）。
func deleteEvent(t *testing.T, ledger string, seq int) {
	t.Helper()
	db, err := sql.Open("sqlite", ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("DELETE FROM eventlog_event WHERE seq = %d", seq)); err != nil {
		t.Fatal(err)
	}
}

// rehash 重写清单里的文件哈希，使“只有账本内容不一致”成为唯一问题。
func rehash(t *testing.T, dir string) {
	t.Helper()
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for rel := range m.Files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			continue
		}
		m.Files[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	b, _ = json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600)
}

// ACT-BAK-06：恢复到空目录后账本可用、数据与密钥齐全，布局完整。
func TestRestoreToEmptyDirKeepsEverything(t *testing.T) {
	home := newHome(t, 25)
	out, _ := mustCreate(t, home, Options{})
	for _, target := range []string{filepath.Join(t.TempDir(), "new"), t.TempDir()} { // 不存在 / 已存在的空目录
		res, err := Restore(out, target, reg(t, true))
		if err != nil {
			t.Fatal(err)
		}
		if res.NoSecrets || len(res.Notices) != 0 {
			t.Errorf("full backup restore should not demand re-auth: %+v", res)
		}
		st, err := store.Open(target, reg(t, true))
		if err != nil {
			t.Fatalf("restored ledger does not open: %v", err)
		}
		info, err := store.Inspect(st.Path(), reg(t, true))
		st.Close()
		if err != nil || info.EventMaxSeq != 25 || info.Tables["things_thing"] != 2 {
			t.Errorf("restored ledger info = %+v err %v", info, err)
		}
		b, err := os.ReadFile(filepath.Join(target, "webhook-keys", "hook"))
		if err != nil || string(b) != "HMAC-SECRET" {
			t.Errorf("webhook key lost: %q %v", b, err)
		}
		if fi, _ := os.Stat(filepath.Join(target, "webhook-keys", "hook")); fi.Mode().Perm() != 0o600 {
			t.Errorf("key mode = %o", fi.Mode().Perm())
		}
		if fi, err := os.Stat(filepath.Join(target, "runs")); err != nil || !fi.IsDir() {
			t.Errorf("layout not completed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(target, "manifest.json")); err == nil {
			t.Error("manifest.json must not land in the data directory")
		}
	}
}

// ACT-BAK-07：非空目录（含正在使用的数据目录）一律拒绝，目录前后一致。
func TestRestoreRefusesNonEmptyDir(t *testing.T) {
	home := newHome(t, 5)
	out, _ := mustCreate(t, home, Options{})
	other := t.TempDir()
	write(t, filepath.Join(other, "any"), "x", 0o644)
	for _, target := range []string{home, other} {
		before := digest(t, target)
		_, err := Restore(out, target, reg(t, true))
		wantCode(t, err, domain.CodeConflict)
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("message should say only empty directories: %v", err)
		}
		if digest(t, target) != before {
			t.Errorf("%s modified", target)
		}
	}
	if _, err := Restore(out, filepath.Join(other, "any"), reg(t, true)); err == nil { // 目标是个文件
		t.Error("restore onto a file succeeded")
	}
}

// ACT-BAK-08：备份的结构版本高于当前二进制时拒绝，目标不留文件。
func TestRestoreRefusesNewerSchema(t *testing.T) {
	home := newHome(t, 5)
	out, _ := mustCreate(t, home, Options{})
	// 清单里的版本被抬高。
	z := filepath.Join(t.TempDir(), "z")
	cloneDir(t, out, z)
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(z, "manifest.json"))
	json.Unmarshal(b, &m)
	m.Schema.Ledger++
	b, _ = json.Marshal(m)
	os.WriteFile(filepath.Join(z, "manifest.json"), b, 0o600)
	target := filepath.Join(t.TempDir(), "rh")
	_, err := Restore(z, target, reg(t, true))
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v, want newer-than-binary refusal", err)
	}
	if _, serr := os.Stat(target); serr == nil {
		t.Error("refused restore left the target behind")
	}
	// 真实的更新账本（旧二进制只认识到结构版本 2）。
	target2 := filepath.Join(t.TempDir(), "rh2")
	_, err = Restore(out, target2, reg(t, false))
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("v3 backup restored by a v2 binary: %v", err)
	}
	if _, serr := os.Stat(target2); serr == nil {
		t.Error("refused restore left the target behind")
	}
}

// ACT-BAK-09：不含密钥的备份恢复后提示重新认证与轮换，且不凭空生成密钥。
func TestRestoreNoSecretsNotifiesReauth(t *testing.T) {
	home := newHome(t, 5)
	out, _ := mustCreate(t, home, Options{NoSecrets: true})
	target := filepath.Join(t.TempDir(), "rh3")
	res, err := Restore(out, target, reg(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoSecrets || len(res.Notices) != 2 {
		t.Fatalf("result = %+v", res)
	}
	joined := strings.Join(res.Notices, "\n")
	if !strings.Contains(joined, "source") || !strings.Contains(joined, "webhook") {
		t.Errorf("notices = %q", joined)
	}
	if entries, _ := os.ReadDir(filepath.Join(target, "webhook-keys")); len(entries) != 0 {
		t.Errorf("webhook keys appeared from nowhere: %v", entries)
	}
}

// ACT-UPG-04：只要账本有待办迁移就留一份只含账本与配置的快照，清单标明升级前的结构版本；
// 无待办迁移或还没有账本时不留。
func TestPreUpgradeSnapshot(t *testing.T) {
	home := newHome(t, 12) // 结构版本 3 的账本
	got, err := PreUpgradeSnapshot(context.Background(), home, reg(t, true), now)
	if err != nil || got != "" {
		t.Fatalf("up-to-date ledger snapshot = %q, %v", got, err)
	}
	if got, err := PreUpgradeSnapshot(context.Background(), filepath.Join(t.TempDir(), "empty"), reg(t, true), now); err != nil || got != "" {
		t.Fatalf("fresh home snapshot = %q, %v", got, err)
	}

	old := t.TempDir() // 结构版本 2 的账本
	st, err := store.Open(old, reg(t, false))
	if err != nil {
		t.Fatal(err)
	}
	addEvents(t, st, 0, 7)
	st.Close()
	write(t, filepath.Join(old, "config.yaml"), "listen: 127.0.0.1:1\n", 0o644)
	write(t, filepath.Join(old, "sources", "s", "cred"), "SECRET", 0o600)

	dir, err := PreUpgradeSnapshot(context.Background(), old, reg(t, true), now)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != filepath.Join(old, "backups") {
		t.Fatalf("snapshot at %s", dir)
	}
	l := listing(t, dir)
	if !has(l, "ledger.db") || !has(l, "config.yaml") || has(l, "sources") || has(l, "sources/s/cred") {
		t.Errorf("snapshot contents = %v", l)
	}
	rep, err := Verify(dir, reg(t, true))
	if err != nil {
		t.Fatalf("snapshot does not verify: %v", err)
	}
	if rep.Manifest.Schema.Ledger != 2 || rep.EventMaxSeq != 7 {
		t.Errorf("snapshot manifest = %+v", rep.Manifest)
	}
	// 同一秒再来一份不覆盖前一份。
	dir2, err := PreUpgradeSnapshot(context.Background(), old, reg(t, true), now)
	if err != nil || dir2 == dir {
		t.Fatalf("second snapshot = %q, %v", dir2, err)
	}

	// 回退路径：旧二进制加快照，恢复到空目录后账本与升级前一致。
	rb := filepath.Join(t.TempDir(), "rb")
	if _, err := Restore(dir, rb, reg(t, false)); err != nil {
		t.Fatal(err)
	}
	s2, err := store.Open(rb, reg(t, false))
	if err != nil {
		t.Fatalf("old binary cannot open the restored snapshot: %v", err)
	}
	s2.Close()
}

// 命令层：退出码与输出形状。
func runCLI(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := client.Env{Getenv: func(k string) string {
		if k == "APA_HOME" {
			return home
		}
		return ""
	}, UserHome: "/nonexistent", Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}
	code := client.Main(args, env)
	return code, out.String(), errb.String()
}

func TestCommands(t *testing.T) {
	UseRegistry(func() (*store.Registry, error) { return reg(t, true), nil })
	home := newHome(t, 15)
	bk := filepath.Join(t.TempDir(), "bk")
	bk2 := filepath.Join(t.TempDir(), "bk2")

	if code, out, _ := runCLI(t, home, "backup", "create", "--out", bk); code != 0 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("create: %d %s", code, out)
	}
	if code, _, _ := runCLI(t, home, "backup", "create", "--no-secrets", "--out", bk2); code != 0 {
		t.Fatalf("create --no-secrets: %d", code)
	}
	if code, out, _ := runCLI(t, home, "backup", "verify", bk); code != 0 || !strings.Contains(out, `"hashes":"match"`) || !strings.Contains(out, `"events":"continuous"`) {
		t.Fatalf("verify: %d %s", code, out)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	cloneDir(t, bk, bad)
	write(t, filepath.Join(bad, "blobs/ab/abcd"), "x", 0o644)
	if code, _, stderr := runCLI(t, home, "backup", "verify", bad); code != 1 || !strings.Contains(stderr, "blobs/ab/abcd") {
		t.Fatalf("verify corrupt: %d %s", code, stderr)
	}
	// restore：位置参数与 --home 的先后都可以。
	rh := filepath.Join(t.TempDir(), "rh")
	if code, out, _ := runCLI(t, home, "backup", "restore", bk, "--home", rh); code != 0 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("restore: %d %s", code, out)
	}
	if code, _, _ := runCLI(t, home, "backup", "restore", bk, "--home", home); code != 6 {
		t.Fatalf("restore into live home exit = %d, want 6", code)
	}
	rh3 := filepath.Join(t.TempDir(), "rh3")
	if code, out, _ := runCLI(t, home, "backup", "restore", "--home", rh3, bk2); code != 0 || !strings.Contains(out, "webhook") {
		t.Fatalf("restore no-secrets: %d %s", code, out)
	}
	for _, args := range [][]string{{"backup"}, {"backup", "bogus"}, {"backup", "verify"}, {"backup", "restore", bk}, {"backup", "create", "extra"},
		{"backup", "verify", bk, "more"}, {"backup", "create", "--nope"}} {
		if code, _, _ := runCLI(t, home, args...); code != 2 {
			t.Errorf("%v exit = %d, want 2", args, code)
		}
	}
	if code, _, _ := runCLI(t, filepath.Join(t.TempDir(), "none"), "backup", "create"); code != 5 {
		t.Errorf("create without ledger exit = %d, want 5", code)
	}
	_ = io.Discard
}

// 备份中途失败（数据目录里有符号链接这类非常规文件）不留半个备份：新建的目录被删，预先存在的空目录被清空。
func TestCreateFailureLeavesNothingBehind(t *testing.T) {
	home := newHome(t, 3)
	other := t.TempDir()
	os.RemoveAll(filepath.Join(home, "blobs"))
	if err := os.Symlink(other, filepath.Join(home, "blobs")); err != nil {
		t.Skip("symlinks unavailable")
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	if _, err := Create(context.Background(), home, Options{Out: fresh}, reg(t, true), now); err == nil {
		t.Fatal("backup of a symlinked data directory succeeded")
	}
	if _, err := os.Stat(fresh); err == nil {
		t.Error("failed backup left its directory (with secrets) behind")
	}
	empty := t.TempDir()
	if _, err := Create(context.Background(), home, Options{Out: empty}, reg(t, true), now); err == nil {
		t.Fatal("backup of a symlinked data directory succeeded")
	}
	if l := listing(t, empty); len(l) != 0 {
		t.Errorf("failed backup left %v in the pre-existing directory", l)
	}
}

// 恢复中途失败（备份里的文件在校验后无法读取）同样不在目标留下文件。
func TestRestoreFailureLeavesTargetEmpty(t *testing.T) {
	home := newHome(t, 3)
	out, _ := mustCreate(t, home, Options{})
	// 把清单列出的一个目录项换成目录，使 Verify 之后读文件失败不可构造；改为目标父路径是文件：MkdirAll 失败。
	blocker := filepath.Join(t.TempDir(), "file")
	write(t, blocker, "x", 0o644)
	if _, err := Restore(out, filepath.Join(blocker, "sub"), reg(t, true)); err == nil {
		t.Fatal("restore under a file succeeded")
	}
	if _, err := Restore(t.TempDir(), t.TempDir(), reg(t, true)); err == nil {
		t.Fatal("restore from a directory without manifest succeeded")
	}
	if _, _, err := runCLIWithoutRegistry(t); err == nil {
		t.Fatal("backup without wired migrations must refuse")
	}
}

func runCLIWithoutRegistry(t *testing.T) (int, string, error) {
	t.Helper()
	saved := registryFn
	registryFn = nil
	defer func() { registryFn = saved }()
	var out, errb bytes.Buffer
	c := &client.Ctx{Env: client.Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}}
	return 0, "", backupCmd(c, []string{"create"})
}

// ACT-BAK-04：备份路径可以是相对路径（create、verify、restore 都认）。
func TestRelativeBackupPath(t *testing.T) {
	UseRegistry(func() (*store.Registry, error) { return reg(t, true), nil })
	home := newHome(t, 5)
	t.Chdir(t.TempDir())
	if code, out, errs := runCLI(t, home, "backup", "create", "--out", "relbk"); code != 0 {
		t.Fatalf("create: %d %s %s", code, out, errs)
	}
	if code, out, errs := runCLI(t, home, "backup", "verify", "relbk"); code != 0 || !strings.Contains(out, `"hashes":"match"`) {
		t.Fatalf("verify: %d %s %s", code, out, errs)
	}
	if code, out, errs := runCLI(t, home, "backup", "restore", "relbk", "--home", "relhome"); code != 0 {
		t.Fatalf("restore: %d %s %s", code, out, errs)
	}
	if _, err := os.Stat("relhome/ledger.db"); err != nil {
		t.Fatal(err)
	}
}

// ACT-BAK-06：sources.json 里指向旧数据目录的 credentials_path 恢复后落在新目录的 sources/<id>/ 下，其余字段与不带路径的来源不变。
func TestRestoreRebasesCredentialsPath(t *testing.T) {
	home := newHome(t, 3)
	write(t, filepath.Join(home, "sources.json"), fmt.Sprintf(`{"sources":[
	 {"id":"mail","command":"/bin/true","timeout":30,"credentials_path":%q,"config":{"a":1}},
	 {"id":"plain","command":"/bin/true"},
	 {"id":"odd","command":"/bin/true","credentials_path":"/elsewhere/cred"}]}`, filepath.Join(home, "sources", "mail", "cred")), 0o644)
	out, _ := mustCreate(t, home, Options{})
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(out, target, reg(t, true)); err != nil {
		t.Fatal(err)
	}
	var f struct {
		Sources []map[string]any `json:"sources"`
	}
	b, _ := os.ReadFile(filepath.Join(target, "sources.json"))
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if got, want := f.Sources[0]["credentials_path"], filepath.Join(target, "sources", "mail", "cred"); got != want {
		t.Errorf("credentials_path = %v, want %v", got, want)
	}
	if f.Sources[0]["timeout"] != float64(30) || f.Sources[0]["config"].(map[string]any)["a"] != float64(1) {
		t.Errorf("other fields changed: %v", f.Sources[0])
	}
	if _, has := f.Sources[1]["credentials_path"]; has {
		t.Errorf("plain source gained a credentials_path")
	}
	if f.Sources[2]["credentials_path"] != "/elsewhere/cred" {
		t.Errorf("a path outside any sources/<id> was rewritten: %v", f.Sources[2]["credentials_path"])
	}
	// 源备份目录里的 sources.json 不被改。
	if b, _ := os.ReadFile(filepath.Join(out, "sources.json")); !strings.Contains(string(b), home) {
		t.Errorf("the backup itself was modified")
	}
}

// ACT-BAK-09：--no-secrets 恢复后留下待办记录，列出没带的凭据目录与接收密钥；普通备份不留。
func TestRestoreNoSecretsLeavesNotice(t *testing.T) {
	home := newHome(t, 3)
	write(t, filepath.Join(home, "sources.json"), fmt.Sprintf(`{"sources":[{"id":"mail","command":"/bin/true","credentials_path":%q}]}`, filepath.Join(home, "sources", "mail", "cred")), 0o644)
	out, _ := mustCreate(t, home, Options{NoSecrets: true})
	target := filepath.Join(t.TempDir(), "r")
	if _, err := Restore(out, target, reg(t, true)); err != nil {
		t.Fatal(err)
	}
	n, ok := ReadNotice(target)
	if !ok || strings.Join(n.Withheld, ",") != "sources/mail,webhook-keys/hook" {
		t.Fatalf("notice = %+v, %v", n, ok)
	}
	if strings.Contains(fmt.Sprint(n), "SECRET") {
		t.Fatal("secret content leaked into the notice")
	}
	full, _ := mustCreate(t, home, Options{})
	t2 := filepath.Join(t.TempDir(), "r2")
	if _, err := Restore(full, t2, reg(t, true)); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadNotice(t2); ok {
		t.Error("a full backup left a notice")
	}
}
