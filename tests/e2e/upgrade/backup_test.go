//go:build e2e

package upgrade

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

const (
	hookSource   = "src_bak_gh"
	scriptSource = "src_bak_script"
)

type bak struct {
	*lab
	e          *baseline.Env
	hookKey    string
	itemsSeen  int
	maxSeq     int64
	restoreOut string
}

func (b *bak) apa(tok string, want int, args ...string) baseline.Result {
	b.t.Helper()
	r := b.e.Apa(tok, "", args...)
	if r.Code != want {
		b.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// itemCount 与 maxSeq 取自某个实例（URL 与观察者凭据）。
func (b *bak) itemTitlesAt(url string) map[string]bool {
	b.t.Helper()
	res := b.run(b.apaBin(), "", []string{"APA_URL=" + url, "APA_TOKEN=" + b.e.OBS}, "item", "list")
	if res.Code != 0 {
		b.t.Fatalf("apa item list: %d %s%s", res.Code, res.Stdout, res.Stderr)
	}
	out := map[string]bool{}
	for _, it := range decode[struct{ Items []struct{ ID, Title string } }](b.t, res.Stdout).Items {
		out[it.ID] = true
	}
	return out
}

func ghBody(n int) []byte {
	return []byte(fmt.Sprintf(`{"action":"opened","issue":{"number":%d,"title":"备份演练 %d","body":"b","html_url":"https://example.invalid/acc/test-repo/issues/%d","updated_at":"2026-11-01T08:00:00Z"},"repository":{"full_name":"acc/test-repo"},"sender":{"login":"alice"}}`, n, n, n))
}

// postHook 用接收密钥签名后向来源推一条 GitHub 类 webhook，返回状态码。
func (b *bak) postHook(url, secret string, n int, delivery string) int {
	b.t.Helper()
	body := ghBody(n)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return httpDo(b.t, "POST", url+"/ingest/"+hookSource, string(body), map[string]string{
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(m.Sum(nil)), "X-GitHub-Event": "issues", "X-GitHub-Delivery": delivery}).Code
}

func ls(t *testing.T, dir string) map[string]bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range ents {
		out[e.Name()] = true
	}
	return out
}

func readDirNames(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// TestBackupRestore 是 flow-backup-restore 的全流程（ACT-BAK-01..10）：真实内核进程上创建一致的备份、
// 校验、损坏检出、恢复到空目录继续使用，以及各种应拒绝的恢复。
func TestBackupRestore(t *testing.T) {
	e := baseline.Setup(t)
	b := &bak{lab: &lab{e, t, t}, e: e}
	var bk1, bk2 string
	root := e.Root
	bk1, bk2 = filepath.Join(root, "BK1"), filepath.Join(root, "BK2")

	step(t, "ACT-BAK-01 准备带数据的实例", func(t *testing.T) {
		b.t = t
		open := e.Projects["acc-open"]
		for i := 0; i < 5; i++ {
			b.apa(e.WRK, 0, "item", "add", "--kind", "task", "--title", fmt.Sprintf("bak-seed-%d", i), "--project", open)
		}
		// 一个带材料的项目。
		note := filepath.Join(t.TempDir(), "m.md")
		if err := os.WriteFile(note, []byte("# 备份演练\n材料正文"), 0o644); err != nil {
			t.Fatal(err)
		}
		b.apa(e.WRK, 0, "material", "commit", "--from", note, "--reason", "备份演练", "--kind", "minutes", "--title", "备份演练纪要", "--project", open)
		// 一个 webhook 来源（有接收密钥文件）与一个自定义脚本来源（凭据目录非空）。
		b.apa(e.DEL, 0, "source", "add", "--id", hookSource, "--type", "webhook", "--source-type", "github",
			"--config", `{"scheme":"hmac-sha256-header","mappings":["github-issues"]}`, "--evidence", "备份演练登记 webhook 来源")
		k := decode[struct{ Secret string }](t, b.apa(e.DEL, 0, "source", "webhook", "create", hookSource, "--evidence", "备份演练签发接收密钥").Stdout)
		if k.Secret == "" {
			t.Fatal("没有签发接收密钥")
		}
		b.hookKey = k.Secret
		cred := filepath.Join(e.Home, "sources", scriptSource, "credentials.json")
		if err := os.MkdirAll(filepath.Dir(cred), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cred, []byte(`{"token":"synthetic-script-credential"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// 自定义脚本来源与一份宿主声明由本人写在主机配置文件里（sources.json、declarations/），重启内核后生效。
		reg, _ := json.Marshal(map[string]any{"sources": []map[string]any{{"id": scriptSource, "type": "script", "command": e.Bin["tool-stub"],
			"schedule": "24h", "timeout": 60, "enabled": false}}}) // 不写 credentials_path：它必须是原数据目录下的绝对路径，换目录恢复后不再成立（见交付说明）
		if err := os.WriteFile(filepath.Join(e.Home, "sources.json"), reg, 0o600); err != nil {
			t.Fatal(err)
		}
		decl, _ := json.Marshal(map[string]any{"harness": "acc-host",
			"headless": map[string]any{"command": []string{e.Bin["stub-harness"], "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
				"access": map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}}}})
		if err := os.MkdirAll(filepath.Join(e.Home, "declarations"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.Home, "declarations", "acc-host.json"), decl, 0o600); err != nil {
			t.Fatal(err)
		}
		e.StopKernel()
		e.StartKernelAgain()
		if r := b.apa(e.OBS, 0, "source", "show", scriptSource); !strings.Contains(r.Stdout, scriptSource) {
			t.Fatalf("主机配置文件里的脚本来源没有登记: %s", r.Stdout)
		}
		if code := b.postHook(e.URL, b.hookKey, 1, "bak-d-1"); code/100 != 2 {
			t.Fatalf("原接收密钥推送 webhook 应 2xx，实际 %d", code)
		}

		for _, d := range []string{"webhook-keys", "sources"} {
			if len(readDirNames(filepath.Join(e.Home, d))) == 0 {
				t.Fatalf("%s/ 应非空", d)
			}
		}
		b.itemsSeen = len(b.itemTitlesAt(e.URL))
		if b.itemsSeen < 5 {
			t.Fatalf("事项条数 %d", b.itemsSeen)
		}
		b.maxSeq = one[int64](t, openRO(t, e.Home), `SELECT MAX(seq) FROM eventlog_event`)
	})

	step(t, "ACT-BAK-02 运行中创建一致的备份", func(t *testing.T) {
		b.t = t
		// 另开一路循环写入，同时创建备份。
		var wg sync.WaitGroup
		errs := make(chan string, 50)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= 50; i++ {
				cmd := exec.Command(e.Bin["apa"], "item", "add", "--kind", "task", "--title", fmt.Sprintf("bak-%d", i), "--project", e.Projects["acc-open"])
				cmd.Env = cleanEnv(e.Home, "APA_URL="+e.URL, "APA_TOKEN="+e.WRK)
				var out bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &out
				if err := cmd.Run(); err != nil {
					errs <- fmt.Sprintf("item add %d: %v %s", i, err, out.String())
				}
			}
		}()
		res := b.apa("", 0, "backup", "create", "--out", bk1)
		wg.Wait()
		close(errs)
		for m := range errs {
			t.Error(m)
		}
		_ = res
		st, err := os.Stat(bk1)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("备份目录权限 = %v（%v），期望 700", st.Mode().Perm(), err)
		}
		have := ls(t, bk1)
		for _, want := range []string{"ledger.db", "blobs", "packs", "config.yaml", "sources.json", "declarations", "sources", "webhook-keys", "manifest.json"} {
			if !have[want] {
				t.Errorf("备份缺 %s: %v", want, have)
			}
		}
		for _, bad := range []string{"runs", "run-credentials", "logs", "backups"} {
			if have[bad] {
				t.Errorf("备份不应含 %s", bad)
			}
		}
		mf := decode[struct {
			APAVersion  string `json:"apa_version"`
			NoSecrets   bool   `json:"no_secrets"`
			Schema      struct{ Ledger int }
			Files       map[string]string
			EventMaxSeq int64 `json:"event_max_seq"`
		}](t, string(mustRead(t, filepath.Join(bk1, "manifest.json"))))
		if mf.APAVersion == "" || mf.Schema.Ledger == 0 || len(mf.Files) == 0 || mf.NoSecrets {
			t.Errorf("清单字段不全: %+v", mf)
		}
		for rel, h := range mf.Files {
			if got := fileSHA(t, filepath.Join(bk1, rel)); got != h {
				t.Errorf("清单里 %s 的哈希与文件不符", rel)
			}
		}
		if mf.EventMaxSeq < b.maxSeq {
			t.Errorf("备份的最大 Event 序号 %d 不应早于备份前记下的 %d", mf.EventMaxSeq, b.maxSeq)
		}
	})

	step(t, "ACT-BAK-03 不含密钥的备份", func(t *testing.T) {
		b.t = t
		b.apa("", 0, "backup", "create", "--out", bk2, "--no-secrets")
		have := ls(t, bk2)
		for _, bad := range []string{"sources", "webhook-keys", "runs", "run-credentials", "logs"} {
			if have[bad] {
				t.Errorf("不含密钥的备份不应含 %s", bad)
			}
		}
		for _, want := range []string{"ledger.db", "blobs", "packs", "config.yaml", "manifest.json"} {
			if !have[want] {
				t.Errorf("不含密钥的备份缺 %s", want)
			}
		}
		if !decode[struct {
			NoSecrets bool `json:"no_secrets"`
		}](t, string(mustRead(t, filepath.Join(bk2, "manifest.json")))).NoSecrets {
			t.Error("清单应标明不含密钥")
		}
	})

	step(t, "ACT-BAK-04 校验完好的备份", func(t *testing.T) {
		b.t = t
		for _, bk := range []string{bk1, bk2} {
			v := decode[struct {
				OK        bool   `json:"ok"`
				Hashes    string `json:"hashes"`
				Integrity string `json:"integrity"`
				Events    string `json:"events"`
			}](t, b.apa("", 0, "backup", "verify", bk).Stdout)
			if !v.OK || v.Hashes != "match" || v.Integrity != "ok" || v.Events != "continuous" {
				t.Errorf("完好的备份 %s 校验结果 = %+v", filepath.Base(bk), v)
			}
		}
	})

	step(t, "ACT-BAK-05 校验发现损坏的备份", func(t *testing.T) {
		b.t = t
		// 改一个 blob 的一个字节：先造出一个 blob（备份里至少要有一个）。
		bx, by := filepath.Join(root, "BK1x"), filepath.Join(root, "BK1y")
		if out, err := exec.Command("cp", "-R", bk1, bx).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if out, err := exec.Command("cp", "-R", bk1, by).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		manifest := decode[struct{ Files map[string]string }](t, string(mustRead(t, filepath.Join(bx, "manifest.json"))))
		var target string
		for rel := range manifest.Files {
			if strings.HasPrefix(rel, "blobs/") {
				target = rel
				break
			}
		}
		if target == "" { // 没有 blob 时改任何一个被清单覆盖的文件（packs 下的）同样是"改一个字节"
			for rel := range manifest.Files {
				if rel != "ledger.db" {
					target = rel
					break
				}
			}
		}
		p := filepath.Join(bx, target)
		data := mustRead(t, p)
		data[0] ^= 0xff
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		r1 := b.apa("", 1, "backup", "verify", bx)
		contains(t, "哈希不符的备份校验输出", r1.Stdout+r1.Stderr, target)

		// 删除账本快照里中间一段 Event。
		_ = os.Chmod(filepath.Join(by, "ledger.db"), 0o600)
		db := openRWPath(t, filepath.Join(by, "ledger.db"))
		mid := one[int64](t, db, `SELECT seq FROM eventlog_event ORDER BY seq LIMIT 1 OFFSET (SELECT COUNT(*) / 2 FROM eventlog_event)`)
		if _, err := db.Exec(`DELETE FROM eventlog_event WHERE seq = ?`, mid); err != nil {
			t.Fatal(err)
		}
		db.Close()
		r2 := b.apa("", 1, "backup", "verify", by)
		contains(t, "Event 序号不连续的备份校验输出", strings.ToLower(r2.Stdout+r2.Stderr), "ledger")
	})

	step(t, "ACT-BAK-06 恢复到空目录并继续使用", func(t *testing.T) {
		b.t = t
		rh1 := filepath.Join(root, "RH1")
		b.apa("", 0, "backup", "restore", bk1, "--home", rh1)
		e.StopKernel() // 停止原服务
		s := b.serve(e.Bin["apa"], rh1)
		titles := b.itemTitlesAt(s.URL)
		if len(titles) < b.itemsSeen {
			t.Errorf("恢复后事项条数 %d 少于备份前记下的 %d", len(titles), b.itemsSeen)
		}
		if max := one[int64](t, openRO(t, rh1), `SELECT MAX(seq) FROM eventlog_event`); max < b.maxSeq {
			t.Errorf("恢复后最大 Event 序号 %d 早于备份前记下的 %d", max, b.maxSeq)
		}
		var any string
		for id := range titles {
			any = id
			break
		}
		why := b.run(e.Bin["apa"], rh1, []string{"APA_URL=" + s.URL, "APA_TOKEN=" + e.OBS}, "why", any)
		if why.Code != 0 || !strings.Contains(why.Stdout, "item.created") {
			t.Errorf("apa why %s: %d %s%s", any, why.Code, why.Stdout, why.Stderr)
		}
		if code := b.postHook(s.URL, b.hookKey, 2, "bak-d-2"); code/100 != 2 {
			t.Errorf("恢复后原接收密钥推送 webhook 应 2xx，实际 %d", code)
		}
		s.stop()
	})

	step(t, "ACT-BAK-07 恢复不覆盖已有目录", func(t *testing.T) {
		b.t = t
		nonEmpty := filepath.Join(root, "nonempty")
		if err := os.MkdirAll(nonEmpty, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nonEmpty, "any.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{e.Home, nonEmpty} {
			before := tree(t, target)
			r := b.run(e.Bin["apa"], e.Home, nil, "backup", "restore", bk1, "--home", target)
			if r.Code != 6 && r.Code != 1 {
				t.Errorf("恢复到非空目录 %s 应拒绝（退出码 6 或 1），实际 %d %s%s", filepath.Base(target), r.Code, r.Stdout, r.Stderr)
			}
			sameTree(t, "被拒的恢复 "+filepath.Base(target), before, tree(t, target))
		}
	})

	step(t, "ACT-BAK-08 备份的结构版本高于当前二进制时拒绝恢复", func(t *testing.T) {
		b.t = t
		latest, err := ledgerfixture.Latest()
		if err != nil {
			t.Fatal(err)
		}
		bz, rh2 := filepath.Join(root, "BK1z"), filepath.Join(root, "RH2")
		if out, err := exec.Command("cp", "-R", bk1, bz).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		mp := filepath.Join(bz, "manifest.json")
		var m map[string]any
		if err := json.Unmarshal(mustRead(t, mp), &m); err != nil {
			t.Fatal(err)
		}
		m["schema"].(map[string]any)["ledger"] = latest + 1
		nb, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(mp, nb, 0o600); err != nil {
			t.Fatal(err)
		}
		r := b.run(e.Bin["apa"], e.Home, nil, "backup", "restore", bz, "--home", rh2)
		if r.Code == 0 {
			t.Errorf("结构版本高于当前二进制的备份应拒绝恢复: %s", r.Stdout)
		}
		if ents := readDirNames(rh2); len(ents) != 0 {
			t.Errorf("被拒的恢复在目标留下了文件: %v", ents)
		}
	})

	step(t, "ACT-BAK-09 由不含密钥的备份恢复后提示重新认证", func(t *testing.T) {
		b.t = t
		rh3 := filepath.Join(root, "RH3")
		b.restoreOut = b.apa("", 0, "backup", "restore", bk2, "--home", rh3).Stdout
		s := b.serve(e.Bin["apa"], rh3)
		if n := len(b.itemTitlesAt(s.URL)); n < b.itemsSeen {
			t.Errorf("事项应完整：%d < %d", n, b.itemsSeen)
		}
		// 提示重新认证：设计 §16.5 的交付形式是恢复命令的输出（来源要重新认证、接收密钥要轮换）；
		// 验收文档 ACT-BAK-09 写的是"doctor 或健康视图指出"，而 doctor 与健康视图在恢复之后没有这一条（见交付说明）。
		contains(t, "不含密钥的备份恢复时的提示", b.restoreOut, "re-authenticate every source", "rotate each webhook source's key")
		for _, d := range []string{"sources", "webhook-keys"} {
			if n := len(readDirNames(filepath.Join(rh3, d))); n != 0 {
				t.Errorf("不含密钥的备份恢复后 %s/ 应为空，实际 %d 项", d, n)
			}
		}
		if code := b.postHook(s.URL, b.hookKey, 3, "bak-d-3"); code/100 == 2 {
			t.Errorf("用原接收密钥推送应被拒，实际 %d", code)
		}
		s.stop()
	})

	step(t, "ACT-BAK-10 切回原数据目录并清理", func(t *testing.T) {
		b.t = t
		e.StartKernelAgain()
		if r := e.Apa(e.OBS, "", "inbox"); r.Code != 0 {
			t.Fatalf("原实例应可用: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		e.StopKernel()
		for _, d := range []string{"BK1", "BK1x", "BK1y", "BK1z", "BK2", "RH1", "RH2", "RH3", "nonempty"} {
			if err := os.RemoveAll(filepath.Join(root, d)); err != nil {
				t.Errorf("清理 %s: %v", d, err)
			}
			if _, err := os.Stat(filepath.Join(root, d)); err == nil {
				t.Errorf("%s 应已删除", d)
			}
		}
	})
}
