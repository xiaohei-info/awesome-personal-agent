package sources

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 附件取回（attachment-fetch，ACT-FET-01/03/04）：事件按真实落盘路径写入，路由、授权、命令与清单更新都是真实的；
// 只有"运行来源程序"由装配方提供（这里是替身，真实子进程在 cli/serve 的测试里跑）。

const fetchAtts = `[{"name":"a.pdf","media_type":"application/pdf","size":9,"ref":"h-1"},{"name":"inline.png","size":3,"inline":true},{"name":"b.txt","size":4,"ref":"h-2","extra":"keep"}]`

type fetchCall struct {
	source, dedupeID, ref, name string
	n                           int
}

// fetchEnv 登记一个声明了 fetch 的来源并落一条带三个附件的事件（第 1 个没有句柄）。
func fetchEnv(t *testing.T) (e *env, id string, calls *[]fetchCall) {
	t.Helper()
	e = setup(t)
	e.registerRaw("src_mail")
	e.canFetch = map[string]bool{"src_mail": true}
	id = e.seedRaw("src_mail", "msg-1", rawEvent{body: "hello", attachments: fetchAtts})
	calls = &[]fetchCall{}
	e.fetcher = func(s domain.Source, ev domain.SourceEvent, n int, att runner.AttachmentRef) ([]byte, error) {
		*calls = append(*calls, fetchCall{s.ID, ev.DedupeKey.EventID, att.Ref, att.Name, n})
		return []byte("content-of-" + att.Name), nil
	}
	return
}

func (e *env) attachmentsPart(id string) (content string, bytes int64, sum string, total int64) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		pd, err := runner.ReadPart(tx, id, runner.PartAttachments)
		if err != nil {
			return err
		}
		content, bytes, sum = string(pd.Content), pd.Bytes, pd.SHA256
		mf, err := runner.ContentOf(tx, id)
		total = mf.Bytes
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) sourceState(id string) (cursor string, events int) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		s, err := registry.Get(tx, id)
		if err != nil {
			return err
		}
		cursor = s.Cursor
		evs, err := runner.Events(tx, id)
		events = len(evs)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func blobFiles(t *testing.T, dir string) int {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

// ACT-FET-01/03：Web 取回成功：200 带更新后的清单项（blob 是内容寻址引用），附件内容在 blob 目录里，
// 清单里该项其余字段原样保留，原文部分的字节数与校验和随之更新，记一条内部审计事件，不产生来源事件、不动游标。
func TestFetchStoresContentAddressedBlobAndRecordsItInTheManifest(t *testing.T) {
	e, id, calls := fetchEnv(t)
	before, _, _, totalBefore := e.attachmentsPart(id)
	cursor, nEvents := e.sourceState("src_mail")

	r := e.web("POST", "/v1/source-events/"+id+"/attachments/2/fetch", "")
	if r.Code != 200 {
		t.Fatalf("fetch: %d %s", r.Code, r.Body)
	}
	var item struct {
		Name, Ref, Blob string
		Size            int64
		Extra           string
	}
	r.into(&item)
	hash := sha("content-of-b.txt")
	if item.Name != "b.txt" || item.Ref != "h-2" || item.Blob != blobs.Ref(hash) || item.Extra != "keep" {
		t.Fatalf("response item: %+v", item)
	}
	if got, err := os.ReadFile(filepath.Join(e.blobs, hash)); err != nil || string(got) != "content-of-b.txt" {
		t.Fatalf("blob file: %q %v", got, err)
	}
	if len(*calls) != 1 || (*calls)[0] != (fetchCall{"src_mail", "msg-1", "h-2", "b.txt", 2}) {
		t.Fatalf("source program must get the program's own event id and the attachment handle: %+v", *calls)
	}

	after, bytes, sum, totalAfter := e.attachmentsPart(id)
	var list []map[string]any
	if err := json.Unmarshal([]byte(after), &list); err != nil || len(list) != 3 || list[2]["blob"] != blobs.Ref(hash) || list[0]["blob"] != nil || list[2]["extra"] != "keep" {
		t.Fatalf("manifest after fetch: %s %v", after, err)
	}
	if bytes != int64(len(after)) || sum != sha(after) || totalAfter != totalBefore+int64(len(after)-len(before)) {
		t.Fatalf("part bytes/sha and event content bytes must follow the update: %d/%d %s total %d→%d", bytes, len(after), sum, totalBefore, totalAfter)
	}
	if n := e.events(EvAttachmentFetched); n != 1 {
		t.Fatalf("want one audit event, got %d", n)
	}
	var payload string
	e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT CAST(payload AS TEXT) FROM eventlog_event WHERE type = ?`, EvAttachmentFetched)
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Scan(&payload)
	})
	var ap struct {
		Event, Name, Blob string
		N                 int
	}
	if json.Unmarshal([]byte(payload), &ap) != nil || ap.Event != id || ap.N != 2 || ap.Name != "b.txt" || ap.Blob != blobs.Ref(hash) {
		t.Fatalf("audit payload: %s", payload)
	}
	if c, n := e.sourceState("src_mail"); c != cursor || n != nEvents {
		t.Fatalf("fetch must not produce source events or move the cursor: cursor %q→%q events %d→%d", cursor, c, nEvents, n)
	}
	// 重复取回同一附件：内容寻址，同一个 blob，不重复存。
	if r := e.web("POST", "/v1/source-events/"+id+"/attachments/2/fetch", ""); r.Code != 200 || blobFiles(t, e.blobs) != 1 {
		t.Fatalf("second fetch: %d, blob files %d", r.Code, blobFiles(t, e.blobs))
	}
}

// 授权：Web 登录与附本人原话的 delegate 可取回；observer、worker、运行凭据不可；delegate 没有本人原话是退出码 23；
// 被拒的调用者不会触发来源程序，也不改清单。
func TestFetchAuthorization(t *testing.T) {
	e, id, calls := fetchEnv(t)
	path := "/v1/source-events/" + id + "/attachments/0/fetch"
	before, _, _, _ := e.attachmentsPart(id)

	denied := map[string]resp{
		"observer":   e.do(e.token(auth.Observer), "POST", path, ""),
		"worker":     e.do(e.token(auth.Worker), "POST", path, ""),
		"run":        e.do(e.runCredential(), "POST", path, ""),
		"no words":   e.do(e.token(auth.Delegate), "POST", path, "", "X-APA-Evidence", ""),
		"not direct": e.do(e.token(auth.Delegate), "POST", path, "", "X-APA-Session-Kind", "group"),
	}
	for name, r := range denied {
		t.Logf("%s: %d %s", name, r.Code, r.code())
		if r.Code != 403 {
			t.Errorf("%s: got %d %s", name, r.Code, r.Body)
		}
	}
	if c := denied["no words"].code(); c != "basis_required" {
		t.Errorf("delegate without the owner's words must be basis_required (exit 23), got %s", c)
	}
	if len(*calls) != 0 {
		t.Fatalf("a denied caller must not start the source program: %+v", *calls)
	}
	if after, _, _, _ := e.attachmentsPart(id); after != before || blobFiles(t, e.blobs) != 0 || e.events(EvAttachmentFetched) != 0 {
		t.Fatal("denied calls must change nothing")
	}

	if r := e.do(e.token(auth.Delegate), "POST", path, ""); r.Code != 200 {
		t.Fatalf("delegate with the owner's words: %d %s", r.Code, r.Body)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls: %+v", *calls)
	}
}

// 来源程序不支持取回、附件没有句柄、位置越界、事件没有附件清单：都不运行来源程序，清单不变。
func TestFetchRejectsWhatCannotBeFetched(t *testing.T) {
	e, id, calls := fetchEnv(t)
	e.registerRaw("src_plain")
	plainID := e.seedRaw("src_plain", "m", rawEvent{body: "x", attachments: fetchAtts}) // 来源没声明 fetch
	noAtt := e.seedRaw("src_mail", "m2", rawEvent{body: "x"})
	for _, c := range []struct {
		name, path string
		code       int
		api        string
	}{
		{"source does not declare fetch", "/v1/source-events/" + plainID + "/attachments/0/fetch", 409, "conflict"},
		{"no handle", "/v1/source-events/" + id + "/attachments/1/fetch", 409, "conflict"},
		{"out of range", "/v1/source-events/" + id + "/attachments/3/fetch", 404, "not_found"},
		{"no attachments part", "/v1/source-events/" + noAtt + "/attachments/0/fetch", 404, "not_found"},
		{"unknown event", "/v1/source-events/sev_nope/attachments/0/fetch", 404, "not_found"},
		{"n is not a number", "/v1/source-events/" + id + "/attachments/x/fetch", 400, "invalid_request"},
		{"n is negative", "/v1/source-events/" + id + "/attachments/-1/fetch", 400, "invalid_request"},
	} {
		if r := e.web("POST", c.path, ""); r.Code != c.code || r.code() != c.api {
			t.Errorf("%s: got %d %s", c.name, r.Code, r.Body)
		}
	}
	if len(*calls) != 0 || blobFiles(t, e.blobs) != 0 || e.events(EvAttachmentFetched) != 0 {
		t.Fatalf("nothing may run or be written: %+v", *calls)
	}
}

// ACT-FET-04：取回失败（来源程序失败、超限）是 502 fetch_failed 并带原因；清单、blob 目录与审计事件都不变。
func TestFetchFailureIsBadGatewayAndLeavesManifestUnchanged(t *testing.T) {
	e, id, _ := fetchEnv(t)
	before, _, _, total := e.attachmentsPart(id)
	e.fetcher = func(domain.Source, domain.SourceEvent, int, runner.AttachmentRef) ([]byte, error) {
		return nil, domain.NewError(domain.CodeFetchFailed, "the source program could not fetch it: message gone", "open the original message instead")
	}
	r := e.web("POST", "/v1/source-events/"+id+"/attachments/0/fetch", "")
	var de domain.Error
	r.into(&de)
	if r.Code != 502 || de.Code != domain.CodeFetchFailed || de.Why == "" || de.ExitCode() != domain.ExitUnclassified {
		t.Fatalf("got %d %s", r.Code, r.Body)
	}
	if after, _, _, t2 := e.attachmentsPart(id); after != before || t2 != total || blobFiles(t, e.blobs) != 0 || e.events(EvAttachmentFetched) != 0 {
		t.Fatal("a failed fetch must leave the manifest, blobs and audit trail unchanged")
	}
	// 非 domain 错误（内部故障）不被包装成 502。
	e.fetcher = func(domain.Source, domain.SourceEvent, int, runner.AttachmentRef) ([]byte, error) {
		return nil, errors.New("boom")
	}
	if r := e.web("POST", "/v1/source-events/"+id+"/attachments/0/fetch", ""); r.Code != 500 {
		t.Fatalf("internal error: %d %s", r.Code, r.Body)
	}
}

// 取回期间事件被物理删除：命令不提交（404），刚存下的 blob 不留孤立文件。
func TestFetchOfAnEventPurgedMeanwhileLeavesNoOrphanBlob(t *testing.T) {
	e, id, _ := fetchEnv(t)
	inner := e.fetcher
	e.fetcher = func(s domain.Source, ev domain.SourceEvent, n int, att runner.AttachmentRef) ([]byte, error) {
		e.update(func(tx *store.Tx) error {
			_, err := tx.Exec(`DELETE FROM sources_runner_event_part WHERE event_id = ?`, id)
			return err
		})
		return inner(s, ev, n, att)
	}
	if r := e.web("POST", "/v1/source-events/"+id+"/attachments/0/fetch", ""); r.Code != 404 {
		t.Fatalf("got %d %s", r.Code, r.Body)
	}
	if blobFiles(t, e.blobs) != 0 || e.events(EvAttachmentFetched) != 0 {
		t.Fatal("no orphan blob and no audit event may remain")
	}
}
