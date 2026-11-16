package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	sregistry "github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	srcdecl "github.com/xiaohei-info/awesome-personal-agent/sources"
)

// 附件取回端到端（attachment-fetch，ACT-FET-01/03/04）：真实装配的内核、真实的路由与命令、真实的外呼调用器与子进程；
// 来源程序是测试目录里的 shell 脚本，接入声明（sourceDecls）在测试里追加一份写了 content.attachments: fetch 的。
func TestAttachmentFetchRunsTheSourceProgramAndStoresTheBlob(t *testing.T) {
	prev := sourceDecls
	sourceDecls = append(append([]srcdecl.Declaration{}, prev...), srcdecl.Declaration{Program: "fetch-mail", Default: srcdecl.Text{Zh: "邮件", En: "Mail"},
		Sender: srcdecl.Sender{Trust: srcdecl.TrustUnverified}, Content: srcdecl.Content{Attachments: srcdecl.AttachmentsFetch}})
	t.Cleanup(func() { sourceDecls = prev })

	k := assembled(t)
	bin := t.TempDir()
	stdinLog := filepath.Join(bin, "stdin.log")
	prog := filepath.Join(bin, "fetch-mail")
	script := `#!/bin/sh
cat > ` + stdinLog + `
[ "$1" = fetch ] && [ "$#" = 1 ] || exit 9
case "$(sed -n 's/.*"ref":"\([^"]*\)".*/\1/p' ` + stdinLog + `)" in
ok) printf 'PK\000\001binary\nno-trailing-newline' ;;
big) head -c 33554433 /dev/zero ;;
gone) echo 'noise' >&2; echo 'message no longer in the mailbox' >&2; exit 3 ;;
esac
`
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	k.do(t, command.Chain{}, sregistry.CmdRegisterExec, map[string]any{"id": "src_fm", "type": "script", "source_type": "mail",
		"command": prog, "config": map[string]any{"account": "gmail"}, "schedule": "1h", "timeout": 10})
	// 另一个来源：程序没有声明 fetch，它的事件不能取回。
	k.do(t, command.Chain{}, sregistry.CmdRegisterExec, map[string]any{"id": "src_plain", "name": "巡检", "type": "script", "source_type": "acc",
		"command": "/bin/sh", "schedule": "1h", "timeout": 10})

	ingest := func(src string) string {
		ev, _ := json.Marshal(map[string]any{"id": "mail-77", "type": "mail.received", "subject": "s", "occurred_at": "2026-01-01T00:00:00Z",
			"attachments": []map[string]any{{"name": "doc.bin", "size": 20, "ref": "ok"}, {"name": "huge.bin", "size": 1, "ref": "big"}, {"name": "old.bin", "size": 1, "ref": "gone"}}})
		code := 0
		res := proc.Result{Run: domain.Run{ID: "run_" + src, Category: domain.RunSource, Target: src, State: domain.RunSucceeded, ExitCode: &code},
			Stdout: []byte(string(ev) + "\n{\"cursor\":\"c1\"}\n")}
		if err := k.rn.Handle(context.Background(), res); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := k.st.View(context.Background(), func(tx *store.Tx) error {
			evs, err := runner.Events(tx, src)
			if len(evs) == 1 {
				id = evs[0].ID
			}
			return err
		}); err != nil || id == "" {
			t.Fatalf("ingest %s: %v", src, err)
		}
		return id
	}
	id := ingest("src_fm")
	plainID := ingest("src_plain")

	web := func(path string) (int, string) {
		login := httptest.NewRequest("POST", "/v1/auth/login", strings.NewReader(`{"password":"`+pw+`"}`))
		login.RemoteAddr = "[::1]:50000"
		lrec := httptest.NewRecorder()
		k.srv.Handler().ServeHTTP(lrec, login)
		var out struct {
			CSRF string `json:"csrf_token"`
		}
		if err := json.Unmarshal(lrec.Body.Bytes(), &out); err != nil || lrec.Code != http.StatusOK {
			t.Fatalf("login: %d %s", lrec.Code, lrec.Body)
		}
		req := httptest.NewRequest("POST", path, nil)
		req.AddCookie(lrec.Result().Cookies()[0])
		req.Header.Set("X-CSRF-Token", out.CSRF)
		rec := httptest.NewRecorder()
		k.srv.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	manifest := func() string {
		var s string
		if err := k.st.View(context.Background(), func(tx *store.Tx) error {
			pd, err := runner.ReadPart(tx, id, runner.PartAttachments)
			s = string(pd.Content)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := manifest()

	// 来源程序以 fetch 为唯一参数运行，stdin 是 {source_id, config, event_id, attachment}；stdout 的字节原样成为附件。
	code, body := web("/v1/source-events/" + id + "/attachments/0/fetch")
	if code != 200 {
		t.Fatalf("fetch: %d %s", code, body)
	}
	var item struct{ Name, Ref, Blob string }
	if err := json.Unmarshal([]byte(body), &item); err != nil || item.Name != "doc.bin" || !strings.HasPrefix(item.Blob, "apa://blob/") {
		t.Fatalf("item: %s %v", body, err)
	}
	want := "PK\x00\x01binary\nno-trailing-newline"
	if got, err := os.ReadFile(filepath.Join(k.layout.Blobs, strings.TrimPrefix(item.Blob, "apa://blob/"))); err != nil || string(got) != want {
		t.Fatalf("blob content %q %v", got, err)
	}
	b, _ := os.ReadFile(stdinLog)
	var in struct {
		SourceID   string          `json:"source_id"`
		Config     json.RawMessage `json:"config"`
		EventID    string          `json:"event_id"`
		Attachment struct{ Ref, Name string }
	}
	if json.Unmarshal(b, &in) != nil || in.SourceID != "src_fm" || string(in.Config) != `{"account":"gmail"}` || in.EventID != "mail-77" || in.Attachment.Ref != "ok" || in.Attachment.Name != "doc.bin" {
		t.Fatalf("stdin: %s", b)
	}
	fetched := manifest()
	if fetched == before || !strings.Contains(fetched, item.Blob) {
		t.Fatalf("manifest must record the blob: %s", fetched)
	}

	// 超过 32 MiB 与来源程序失败：502，原因写明；清单不变，不留附件。
	for _, c := range []struct{ n, why string }{{"1", "32 MiB"}, {"2", "message no longer in the mailbox"}} {
		code, body := web("/v1/source-events/" + id + "/attachments/" + c.n + "/fetch")
		var de domain.Error
		if json.Unmarshal([]byte(body), &de) != nil || code != 502 || de.Code != domain.CodeFetchFailed || !strings.Contains(de.Why, c.why) {
			t.Fatalf("attachment %s: %d %s", c.n, code, body)
		}
	}
	if manifest() != fetched {
		t.Fatal("failed fetches must leave the manifest unchanged")
	}
	if es, _ := os.ReadDir(k.layout.Blobs); len(es) != 1 {
		t.Fatalf("failed fetches must not store attachments, blobs dir has %d files", len(es))
	}

	// 程序没声明 fetch：409，不运行程序。
	if code, body := web("/v1/source-events/" + plainID + "/attachments/0/fetch"); code != 409 {
		t.Fatalf("undeclared: %d %s", code, body)
	}

	// 取回不产生来源事件、不推进游标。
	if err := k.st.View(context.Background(), func(tx *store.Tx) error {
		evs, err := runner.Events(tx, "src_fm")
		if err != nil || len(evs) != 1 {
			t.Fatalf("events: %v %v", evs, err)
		}
		s, err := sregistry.Get(tx, "src_fm")
		if s.Cursor != "c1" {
			t.Fatalf("cursor moved: %q", s.Cursor)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
