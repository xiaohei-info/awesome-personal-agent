//go:build e2e

package ops

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const marker = "MARKER-PURGE"

type purged struct {
	Object        string   `json:"object"`
	Kind          string   `json:"kind"`
	Events        int64    `json:"events"`
	BlobsRemoved  []string `json:"blobs_removed"`
	BlobsRetained []string `json:"blobs_retained"`
}

type whyStep struct {
	Seq       int64                                 `json:"seq"`
	Type      string                                `json:"type"`
	Object    string                                `json:"object"`
	Reason    string                                `json:"reason"`
	Evidence  string                                `json:"evidence"`
	Tombstone bool                                  `json:"tombstone"`
	Actor     struct{ Type, Principal, Via string } `json:"actor"`
}

func (f *fx) why(token, id string) (string, []whyStep) {
	f.t.Helper()
	r := f.apa(token, 0, "why", id)
	return r.Stdout, decode[struct {
		Steps []whyStep `json:"steps"`
	}](f.t, r.Stdout).Steps
}

func (f *fx) itemExists(id string) bool {
	return f.Apa(f.OBS, "", "item", "show", id).Code == 0
}

// blobPut 以 WRK 上传一个附件，返回 sha256。
func (f *fx) blobPut(content string) string {
	f.t.Helper()
	r := f.apa(f.WRK, 0, "blob", "put", f.file("blob.bin", content))
	out := decode[struct {
		Hash   string `json:"hash"`
		Sha256 string `json:"sha256"`
	}](f.t, r.Stdout)
	if out.Hash != "" {
		return out.Hash
	}
	if out.Sha256 == "" {
		f.t.Fatalf("blob put 没有返回哈希: %s", r.Stdout)
	}
	return out.Sha256
}

func (f *fx) blobFile(hash string) string { return filepath.Join(f.Home, "blobs", hash) }

func (f *fx) blobExists(hash string) bool {
	found := false
	_ = filepath.Walk(filepath.Join(f.Home, "blobs"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(filepath.Base(p), hash) {
			found = true
		}
		return nil
	})
	return found
}

// material 以 WRK 新建一份自有型材料，正文 body 里可带 apa://blob/<hash> 引用。
func (f *fx) material(title, body string) string {
	f.t.Helper()
	r := f.apa(f.WRK, 0, "material", "commit", "--kind", "minutes", "--title", title, "--project", f.Projects["acc-open"],
		"--reason", "验收造数", "--from", f.file("m.md", body))
	return decode[struct {
		Material string `json:"material"`
	}](f.t, r.Stdout).Material
}

// TestFlowPurge 对应验收文档 flow-purge（ACT-PRG-01..05）。
func TestFlowPurge(t *testing.T) {
	f := newFx(t)
	var (
		itemID, mat1, mat2, mat3 string
		before                   []event
		cursor                   int64
		blob1, blob2             string
	)

	step(t, "ACT-PRG-01 物理删除事项与材料，内容不可再搜到", func(t *testing.T) {
		r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "待删事项", "--phase", "open", "--project", f.Projects["acc-open"],
			"--body", "正文 "+marker, "--evidence", "证据 "+marker)
		itemID = decode[struct {
			ID string `json:"id"`
		}](t, r.Stdout).ID
		blob1 = f.blobPut("附件 " + marker)
		blob2 = f.blobPut("共享附件 KEEP-BLOB")
		mat1 = f.material("待删材料", "材料正文 "+marker+"\n![附件](apa://blob/"+blob1+")\n")
		mat2 = f.material("共享材料一", "第一份引用共享附件 apa://blob/"+blob2+"\n")
		mat3 = f.material("共享材料二", "第二份也引用共享附件 apa://blob/"+blob2+"\n")

		if len(f.grepHome(marker)) == 0 {
			t.Fatal("造数后 APA_HOME 里应能搜到标记，否则后面的 grep 没有意义")
		}
		before = f.feed(0)
		cursor = before[len(before)-1].Seq
		var itemSeqs []int64
		for _, e := range before {
			if e.Object == itemID {
				itemSeqs = append(itemSeqs, e.Seq)
			}
		}
		if len(itemSeqs) == 0 {
			t.Fatal("事项应有 Event")
		}

		res := decode[purged](t, f.webOK("POST", "/settings/purge", `{"object":"`+itemID+`"}`, http.StatusOK))
		eq(t, "删除的种类", res.Kind, "item")
		if res.Events < 1 {
			t.Errorf("应有事项 Event 被墓碑化: %+v", res)
		}
		if f.itemExists(itemID) {
			t.Error("事项投影应已删除")
		}

		res = decode[purged](t, f.webOK("POST", "/settings/purge", `{"object":"`+mat1+`"}`, http.StatusOK))
		eq(t, "删除的种类", res.Kind, "material")
		eq(t, "无其他引用的 blob 被删除", strings.Join(res.BlobsRemoved, ","), blob1)
		if f.blobExists(blob1) {
			t.Error("无其他引用的 blob 文件应已删除")
		}
		if f.Apa(f.OBS, "", "material", "show", mat1).Code == 0 {
			t.Error("材料投影应已删除")
		}

		// Event 保留序号、类型、对象 id、时间、actor，内容字段替换为墓碑；另记一条删除事件。
		_, steps := f.why(f.DEL, itemID)
		var created, deletedEv bool
		for _, s := range steps {
			if s.Type == "purge.executed" {
				deletedEv = true
				continue
			}
			if !s.Tombstone || s.Reason != "" || s.Evidence != "" {
				t.Errorf("被删对象的 Event 应为墓碑且无内容: %+v", s)
			}
			if s.Actor.Type == "" || s.Object != itemID {
				t.Errorf("墓碑应保留 actor 与对象 id: %+v", s)
			}
			created = created || s.Type == "item.created"
		}
		if !created || !deletedEv {
			t.Errorf("应保留 item.created 的信封并另记 purge.executed: %+v", steps)
		}

		// 账本之外的文件（blob、运行目录等）此时就不应再含被删内容；账本在最后一步清理后检查。
		for _, h := range f.grepHome(marker) {
			if filepath.Base(h) != "ledger.db" && !strings.HasSuffix(h, "ledger.db-wal") {
				t.Errorf("物理删除后仍能在 %s 搜到 %s", h, marker)
			}
		}
	})

	step(t, "ACT-PRG-02 仍有引用的 blob 保留", func(t *testing.T) {
		res := decode[purged](t, f.webOK("POST", "/settings/purge", `{"object":"`+mat2+`"}`, http.StatusOK))
		eq(t, "保留的 blob", strings.Join(res.BlobsRetained, ","), blob2)
		eq(t, "被删除的 blob 数", len(res.BlobsRemoved), 0)
		if !f.blobExists(blob2) {
			t.Fatal("blob 被误删")
		}
		out := f.apa(f.OBS, 0, "material", "show", mat3).Stdout
		if !strings.Contains(out, "第二份也引用共享附件") {
			t.Errorf("另一材料应仍可读取: %s", out)
		}
		out2 := filepath.Join(t.TempDir(), "blob.out")
		f.apa(f.OBS, 0, "blob", "get", blob2, "--out", out2)
		if b, err := os.ReadFile(out2); err != nil || string(b) != "共享附件 KEEP-BLOB" {
			t.Errorf("共享 blob 应仍可读取: %q %v", b, err)
		}
	})

	step(t, "ACT-PRG-03 游标不受影响，墓碑不泄露内容", func(t *testing.T) {
		after := f.feed(0)
		// 删除前已有的信封序号全部保留且顺序不变。
		if len(after) < len(before) {
			t.Fatalf("事件流变短: 删除前 %d 条，删除后 %d 条", len(before), len(after))
		}
		for i, e := range before {
			if after[i].Seq != e.Seq || after[i].Type != e.Type || after[i].Object != e.Object {
				t.Fatalf("第 %d 条信封被改动: %+v → %+v", i, e, after[i])
			}
		}
		for i := 1; i < len(after); i++ {
			if after[i].Seq <= after[i-1].Seq {
				t.Errorf("序号应严格递增: %d 后是 %d", after[i-1].Seq, after[i].Seq)
			}
		}
		for _, e := range f.feed(cursor) {
			if e.Seq <= cursor {
				t.Errorf("--cursor %d 之后不应再给出 %d", cursor, e.Seq)
			}
		}
		raw := f.apa(f.OBS, 0, "events", "--cursor", "0").Stdout
		if strings.Contains(raw, marker) {
			t.Error("事件流泄露了被删内容")
		}
		for _, tok := range []string{f.OBS, f.DEL} {
			out, _ := f.why(tok, itemID)
			if strings.Contains(out, marker) {
				t.Errorf("apa why 泄露了被删内容: %s", out)
			}
		}
		out := f.webOK("GET", "/why/"+itemID, "", http.StatusOK)
		if strings.Contains(out, marker) || !strings.Contains(out, `"tombstone":true`) {
			t.Errorf("Web 时间线应显示墓碑且不含内容: %s", out)
		}
	})

	step(t, "ACT-PRG-04 物理删除只在 Web", func(t *testing.T) {
		keep := f.material("不可被代删", "保留 KEEP-MATERIAL")
		for _, c := range []struct{ name, token, code string }{
			{"DEL", f.DEL, "web_only"}, {"WRK", f.WRK, "permission_denied"}, {"OBS", f.OBS, "permission_denied"},
		} {
			r := f.APIRaw("POST", "/v1/settings/purge", `{"object":"`+keep+`"}`, c.token)
			if r.Code != http.StatusForbidden || !strings.Contains(r.Body, `"code":"`+c.code+`"`) {
				t.Errorf("%s 请求物理删除应被拒绝（403 %s）: %d %s", c.name, c.code, r.Code, r.Body)
			}
		}
		if !strings.Contains(f.apa(f.OBS, 0, "material", "show", keep).Stdout, "KEEP-MATERIAL") {
			t.Error("被拒绝后内容应不变")
		}
	})

	step(t, "ACT-PRG-05 墓碑不计入统计", func(t *testing.T) {
		const judge = "这条来源事件是否值得成为建议" // inbound-triage/triage.md（判断型，声明 event_stats）
		// 来源事件经 triage-screen：判断端点落在 band 内（0.15–0.60）才升级到对话端点，统计只出现在对话端点的请求里。
		f.WireEndpoints()
		f.ModelStub.Reset()
		f.Cls.PYes(modelstub.Key{Contains: judge}, 0.40)
		f.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: judge}, Answer: "yes"})
		q := filepath.Join(f.Root, "prg05-source")
		for _, d := range []string{"in", "done"} {
			if err := os.MkdirAll(filepath.Join(q, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		script := "#!/bin/sh\nQ=\"$1\"\ncat > /dev/null\nfor f in \"$Q\"/in/*.jsonl; do\n  [ -e \"$f\" ] || continue\n  cat \"$f\"\n  mv \"$f\" \"$Q/done/\"\ndone\necho '{\"cursor\":\"c'\"$(ls \"$Q/done\" | wc -l | tr -d ' ')\"'\"}'\n"
		if err := os.WriteFile(filepath.Join(q, "src.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		reg, _ := json.Marshal(map[string]any{"id": "src_prg05", "type": "script", "source_type": "acc",
			"command": filepath.Join(q, "src.sh") + " " + q, "schedule": "1s", "timeout": 30, "default_project": f.Projects["acc-open"]})
		f.webOK("POST", "/sources", string(reg), http.StatusCreated)
		seq := 0
		propose := func(text, _ string) string {
			t.Helper()
			known := f.itemIDs()
			seq++
			ev, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("prg05-%d", seq), "type": "acc.msg", "subject": text, "summary": "摘要 " + text,
				"detail": "正文 " + text, "sender": "prg05@example.invalid", "sender_verified": true, "occurred_at": time.Now().UTC().Format(time.RFC3339)})
			tmp := filepath.Join(q, "tmp")
			if err := os.WriteFile(tmp, append(ev, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, filepath.Join(q, "in", fmt.Sprintf("%04d.jsonl", seq))); err != nil {
				t.Fatal(err)
			}
			var id string
			f.waitFor("新建议入账: "+text, 30*time.Second, func() bool {
				for k := range f.itemIDs() {
					if !known[k] {
						id = k
						return true
					}
				}
				return false
			})
			return id
		}
		dropped := propose("PRG05 要被丢弃的建议", "s1")
		f.webOK("POST", "/items/"+dropped+"/dismiss", `{"reason":"不要"}`, http.StatusOK)

		lastReq := func() string {
			reqs := f.ModelStub.Requests()
			for i := len(reqs) - 1; i >= 0; i-- { // 起草请求在判断之后，不带统计
				if strings.Contains(reqs[i].BodyText, judge) {
					return reqs[i].BodyText
				}
			}
			t.Fatal("对话端点没有收到判断题")
			return ""
		}
		propose("PRG05 丢弃后再来一条", "s2")
		if dismissedIn(lastReq()) != "1" {
			t.Fatalf("物理删除前，统计应计入已丢弃的建议: %s", lastReq())
		}

		f.webOK("POST", "/settings/purge", `{"object":"`+dropped+`"}`, http.StatusOK)
		propose("PRG05 删除后再来一条", "s3")
		if got := lastReq(); dismissedIn(got) != "0" {
			t.Errorf("物理删除后，统计不应再计该条: %s", got)
		}
	})

	step(t, "ACT-PRG-01 清理 SQLite 后 grep 无匹配", func(t *testing.T) {
		// 删除的行会残留在 SQLite 空闲页里，验收文档规定"SQLite 文件需完成清理后检查"：
		// 停内核、对账本做 VACUUM（同时完成 WAL 检查点），再 grep 整个 APA_HOME。
		f.StopKernel()
		f.vacuumLedger()
		for _, h := range f.grepHome(marker) {
			t.Errorf("物理删除并清理后仍能在 %s 搜到 %s", h, marker)
		}
		if len(f.grepHome("KEEP-BLOB")) == 0 || len(f.grepHome("KEEP-MATERIAL")) == 0 {
			t.Error("未删除的内容应仍在（共享 blob、被拒绝删除的材料）")
		}
	})
}

// dismissedIn 取判断题输入里 stats.dismissed 的值（输入是 JSON，请求体里引号被转义）。
func dismissedIn(body string) string {
	m := regexp.MustCompile(`dismissed\\*":(\d+)`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}
