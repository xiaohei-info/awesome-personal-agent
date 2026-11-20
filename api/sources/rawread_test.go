package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 来源原文读取（raw-read-api）：来源事件的原文各部分按真实落盘路径写入（runner.AppendInternal 写正文，
// 其余部分按与 runner.storeParts 相同的行形状写入原文表），读取走真实路由与授权。

var bg = context.Background()

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type rawEvent struct {
	body, original, headers, attachments, messages string
}

func (e *env) update(fn func(tx *store.Tx) error) {
	e.t.Helper()
	if err := e.st.Update(bg, fn); err != nil {
		e.t.Fatal(err)
	}
}

// seedRaw 落一条带全部原文部分的事件并返回 id。
func (e *env) seedRaw(source, key string, r rawEvent) string {
	e.t.Helper()
	var id string
	e.update(func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, domain.Source{ID: source}, domain.SourceEvent{ID: key, Type: "mail.received", Subject: "subj-" + key, Summary: "summary-" + key,
			Detail: r.body, Link: "https://mail.example/path?token=secret", Sender: "alice", OccurredAt: e.now}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, source)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.DedupeKey.EventID == key {
				id = ev.ID
			}
		}
		total := int64(len(r.body))
		for _, p := range []struct{ part, media, content string }{
			{runner.PartOriginal, "text/html", r.original}, {runner.PartHeaders, "application/json", r.headers},
			{runner.PartAttachments, "application/json", r.attachments}, {runner.PartMessages, "application/json", r.messages},
		} {
			if p.content == "" {
				continue
			}
			total += int64(len(p.content))
			if _, err := tx.Exec(`INSERT INTO sources_runner_event_part (event_id, part, media_type, bytes, sha256, content) VALUES (?,?,?,?,?,?)`,
				id, p.part, p.media, int64(len(p.content)), sha(p.content), []byte(p.content)); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE sources_runner_event SET content_bytes = ? WHERE id = ?`, total, id)
		return err
	})
	return id
}

// seedLegacy 落一条升级前的事件：全文在 detail 与 messages 旧列，没有原文部分，字节数为空，状态 legacy。
func (e *env) seedLegacy(source, key, detail, messages string) string {
	e.t.Helper()
	id := e.seedRaw(source, key, rawEvent{body: "x"})
	e.update(func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sources_runner_event_part WHERE event_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE sources_runner_event SET detail = ?, messages = ?, content_bytes = NULL, content_state = 'legacy' WHERE id = ?`, detail, messages, id)
		return err
	})
	return id
}

func (e *env) setExternal(source, scope string) {
	e.t.Helper()
	ext := `null`
	if scope != "" {
		ext = `"` + scope + `"`
	}
	if _, err := e.x.Do(bg, command.Request{Identity: webIdentity, Command: registry.CmdSetOutbound,
		Input: json.RawMessage(`{"id":"` + source + `","external":` + ext + `}`)}); err != nil {
		e.t.Fatal(err)
	}
}

var webIdentity = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}

type readResp struct {
	Event struct {
		domain.SourceEvent
		Content runner.Manifest `json:"content"`
	} `json:"event"`
	Content     runner.Manifest  `json:"content"`
	Body        *string          `json:"body"`
	Original    *runner.Original `json:"original"`
	Headers     json.RawMessage  `json:"headers"`
	Attachments json.RawMessage  `json:"attachments"`
	Messages    json.RawMessage  `json:"messages"`
	Scope       string           `json:"scope"`
	Trimmed     *trimmed         `json:"trimmed"`
}

func read(t *testing.T, r resp) readResp {
	t.Helper()
	if r.Code != 200 {
		t.Fatalf("read: %d %s", r.Code, r.Body)
	}
	var out readResp
	r.into(&out)
	return out
}

func (e *env) registerRaw(ids ...string) {
	e.t.Helper()
	for _, id := range ids {
		if r := e.web("POST", "/v1/sources", `{"id":"`+id+`","type":"webhook","source_type":"mail","config":{"scheme":"shared"}}`); r.Code != 201 {
			e.t.Fatalf("register %s: %d %s", id, r.Code, r.Body)
		}
	}
}

var (
	bigBody = strings.Repeat("长正文abc", 700) // 4200 个字符
	rawFull = rawEvent{body: bigBody, original: "<p>" + strings.Repeat("h", 3000) + "</p>", headers: `{"From":"a@x.example"}`,
		attachments: `[{"name":"a.pdf","size":2048}]`, messages: `[{"sender":"bob","text":"hi","at":"2026-03-01T00:00:00Z"}]`}
)

// ACT-RSC-02：Web 登录读到全文，scope 恒为 full，无 trimmed；带 part 时只回那一部分。
func TestWebReadsFullContentAndPartSelectsOne(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_raw")
	id := e.seedRaw("src_raw", "k1", rawFull)
	e.setExternal("src_raw", "none") // 外发范围不约束本人

	got := read(t, e.web("GET", "/v1/source-events/"+id, ""))
	if got.Scope != "full" || got.Trimmed != nil || got.Body == nil || *got.Body != bigBody {
		t.Fatalf("web must read the full body: scope=%s trimmed=%v", got.Scope, got.Trimmed)
	}
	if got.Original == nil || got.Original.MediaType != "text/html" || got.Original.Text != rawFull.original ||
		string(got.Headers) != rawFull.headers || string(got.Attachments) != rawFull.attachments || string(got.Messages) != rawFull.messages {
		t.Fatalf("web must read every part: %+v", got)
	}
	if got.Content.State != "full" || len(got.Content.Parts) != 5 || got.Content.Bytes != int64(len(rawFull.body)+len(rawFull.original)+len(rawFull.headers)+len(rawFull.attachments)+len(rawFull.messages)) {
		t.Fatalf("manifest: %+v", got.Content)
	}
	if utf8.RuneCountInString(got.Event.Detail) != 2000 || !strings.HasPrefix(bigBody, got.Event.Detail) || got.Event.Content.Bytes != got.Content.Bytes {
		t.Fatalf("event must carry a 2000-char preview and the manifest, got %d chars", utf8.RuneCountInString(got.Event.Detail))
	}

	hdr := read(t, e.web("GET", "/v1/source-events/"+id+"?part=headers", ""))
	if string(hdr.Headers) != rawFull.headers || hdr.Body != nil || hdr.Original != nil || hdr.Attachments != nil || hdr.Messages != nil {
		t.Fatalf("?part=headers must return only headers: %+v", hdr)
	}
	if r := e.web("GET", "/v1/source-events/"+id+"?part=bogus", ""); r.Code != 400 {
		t.Errorf("unknown part: %d", r.Code)
	}
	if r := e.web("GET", "/v1/source-events/sev_nope", ""); r.Code != 404 {
		t.Errorf("unknown event: %d", r.Code)
	}
	plain := e.seedRaw("src_raw", "k2", rawEvent{body: "short"})
	if r := e.web("GET", "/v1/source-events/"+plain+"?part=original", ""); r.Code != 404 {
		t.Errorf("a part the event does not have: %d %s", r.Code, r.Body)
	}
}

// ACT-RSC-03、04：token 读到的内容按该来源的 outbound.external 裁剪；未选择、none 与 meta 一样只回元信息，scope 是 meta。
func TestTokenReadIsTrimmedToExternalOutboundScope(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_full", "src_ex", "src_meta", "src_none", "src_unset")
	ids := map[string]string{}
	for _, s := range []string{"src_full", "src_ex", "src_meta", "src_none", "src_unset"} {
		ids[s] = e.seedRaw(s, "k", rawFull)
	}
	e.setExternal("src_full", "full")
	e.setExternal("src_ex", "excerpt")
	e.setExternal("src_meta", "meta")
	e.setExternal("src_none", "none")
	obs := e.token(auth.Observer)

	full := read(t, e.do(obs, "GET", "/v1/source-events/"+ids["src_full"], ""))
	if full.Scope != "full" || full.Trimmed != nil || full.Body == nil || *full.Body != bigBody || full.Original == nil || full.Headers == nil || full.Messages == nil {
		t.Fatalf("full: %+v", full)
	}

	ex := read(t, e.do(obs, "GET", "/v1/source-events/"+ids["src_ex"], ""))
	wantBody := string([]rune(bigBody)[:500])
	if ex.Scope != "excerpt" || ex.Body == nil || *ex.Body != wantBody || ex.Trimmed == nil || ex.Trimmed.TotalChars != 4200 || ex.Trimmed.ReturnedChars != 500 {
		t.Fatalf("excerpt body: %+v trimmed=%+v", ex.Body != nil, ex.Trimmed)
	}
	if ex.Original != nil || ex.Headers != nil || ex.Messages != nil || ex.Event.Summary != "summary-k" || utf8.RuneCountInString(ex.Event.Detail) != 500 || ex.Event.Link != "mail.example" {
		t.Fatalf("excerpt must hold back original, headers, messages and keep only the link domain: %+v", ex)
	}
	if exOne := read(t, e.do(obs, "GET", "/v1/source-events/"+ids["src_ex"]+"?part=original", "")); exOne.Original != nil {
		t.Fatalf("?part=original under excerpt must not return the original")
	}

	for _, s := range []string{"src_meta", "src_none", "src_unset"} {
		m := read(t, e.do(obs, "GET", "/v1/source-events/"+ids[s], ""))
		if m.Scope != "meta" || m.Body != nil || m.Original != nil || m.Headers != nil || m.Attachments != nil || m.Messages != nil {
			t.Fatalf("%s must return metadata only: %+v", s, m)
		}
		if m.Event.Subject != "subj-k" || m.Event.Sender != "alice" || m.Event.Summary != "" || m.Event.Detail != "" || m.Event.Link != "mail.example" ||
			len(m.Content.Parts) != 5 || m.Trimmed == nil || m.Trimmed.TotalChars != 4200 || m.Trimmed.ReturnedChars != 0 {
			t.Fatalf("%s meta event/manifest: %+v trimmed=%+v", s, m.Event, m.Trimmed)
		}
	}
	// 本人在 Web 始终全文，不受范围影响。
	if w := read(t, e.web("GET", "/v1/source-events/"+ids["src_none"], "")); w.Scope != "full" || w.Body == nil || *w.Body != bigBody {
		t.Fatalf("web under none: %+v", w)
	}
}

// ACT-RSC-05、07：运行凭据、ext 与 webhook 密钥读不到来源原文；对话捕获的内部来源只对 Web 登录开放。
func TestRunCredentialExtAndCaptureCannotReadContent(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_raw")
	e.setExternal("src_raw", "full")
	id := e.seedRaw("src_raw", "k", rawFull)
	path := "/v1/source-events/" + id
	for name, tok := range map[string]string{"run": e.runCredential(), "ext": e.token(auth.Ext), "webhook": e.token(auth.Webhook)} {
		if r := e.do(tok, "GET", path, ""); r.Code != 403 {
			t.Errorf("%s credential: %d %s", name, r.Code, r.Body)
		}
	}
	capID := e.seedRaw("capture:prj_1", "c1", rawEvent{body: "对话增量"})
	if r := e.do(e.token(auth.Observer), "GET", "/v1/source-events/"+capID, ""); r.Code != 403 {
		t.Errorf("observer reading conversation capture: %d %s", r.Code, r.Body)
	}
	if r := e.do(e.token(auth.Delegate), "GET", "/v1/source-events/"+capID, ""); r.Code != 403 {
		t.Errorf("delegate reading conversation capture: %d", r.Code)
	}
	if got := read(t, e.web("GET", "/v1/source-events/"+capID, "")); got.Body == nil || *got.Body != "对话增量" || got.Scope != "full" {
		t.Errorf("web reading conversation capture: %+v", got)
	}
}

// ACT-RSC-06：挂在已接受事项上的挂靠消息按 GET /items/{id}/messages 的权限读，不因 none 被裁成元信息；
// 运行凭据只经转发白名单读它（发送者不在名单时 403）。
func TestAttachedMessageIsReadWithItsOwnPermissionNotOutboundScope(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_none")
	e.setExternal("src_none", "none")
	id := e.seedRaw("src_none", "reply", rawFull)
	res, err := e.x.Do(bg, command.Request{Identity: webIdentity, Command: "item.create", Input: json.RawMessage(`{"kind":"task","title":"t"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var it struct{ ID string }
	if err := json.Unmarshal(res.Value, &it); err != nil {
		t.Fatal(err)
	}
	obs := e.token(auth.Observer)
	if m := read(t, e.do(obs, "GET", "/v1/source-events/"+id, "")); m.Scope != "meta" {
		t.Fatalf("a message that is not attached follows the outbound scope: %s", m.Scope)
	}
	e.update(func(tx *store.Tx) error { return runner.SetAttachState(tx, id, runner.AttachPending, it.ID) })

	got := read(t, e.do(obs, "GET", "/v1/source-events/"+id, ""))
	if got.Scope != "full" || got.Body == nil || *got.Body != bigBody || got.Trimmed != nil {
		t.Fatalf("attached message must be readable in full by a token: %+v", got.Scope)
	}
	if r := e.do(e.runCredential(), "GET", "/v1/source-events/"+id, ""); r.Code != 403 {
		t.Errorf("run credential reads attached messages only through the forward allowlist: %d %s", r.Code, r.Body)
	}
	if r := e.do(e.token(auth.Ext), "GET", "/v1/source-events/"+id, ""); r.Code != 403 {
		t.Errorf("ext: %d", r.Code)
	}

	// GET /items/{id}/messages：detail 是预览，另带 content 清单，不带各部分内容与聚合消息。
	var list struct {
		Messages []struct {
			ID      string          `json:"id"`
			Detail  string          `json:"detail"`
			Content runner.Manifest `json:"content"`
		} `json:"messages"`
	}
	lr := e.do(obs, "GET", "/v1/items/"+it.ID+"/messages", "")
	lr.into(&list)
	if lr.Code != 200 || len(list.Messages) != 1 {
		t.Fatalf("messages: %d %s", lr.Code, lr.Body)
	}
	m := list.Messages[0]
	if utf8.RuneCountInString(m.Detail) != 2000 || !strings.HasPrefix(bigBody, m.Detail) || m.Content.State != "full" || len(m.Content.Parts) != 5 {
		t.Fatalf("message must carry a 2000-char preview and a manifest: %d chars %+v", utf8.RuneCountInString(m.Detail), m.Content)
	}
	if strings.Contains(lr.Body.String(), "hi\"") || strings.Contains(lr.Body.String(), rawFull.original) {
		t.Fatal("message list must not carry part contents")
	}
}

// ACT-RAW-04：来源事件列表只带 2000 字符的正文开头与原文清单，不带聚合消息内容；全文经单条读取。
// 写命令（忽略）的响应同口径。
func TestEventListsCarryPreviewAndManifestOnly(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_raw")
	id := e.seedRaw("src_raw", "big", rawFull)
	e.seedRaw("src_raw", "small", rawEvent{body: "tiny"})
	e.update(func(tx *store.Tx) error {
		return runner.SetDisposition(tx, id, domain.DispositionUnjudged, "", "")
	})
	type listed struct {
		Events []struct {
			ID      string          `json:"id"`
			Detail  string          `json:"detail"`
			Content runner.Manifest `json:"content"`
		} `json:"events"`
	}
	obs := e.token(auth.Observer)
	for _, q := range []string{"", "?sort=recent", "?sort=score"} {
		r := e.do(obs, "GET", "/v1/sources/src_raw/events"+q, "")
		var l listed
		r.into(&l)
		if r.Code != 200 || len(l.Events) != 2 {
			t.Fatalf("%q: %d %s", q, r.Code, r.Body)
		}
		for _, ev := range l.Events {
			switch ev.Detail {
			case "tiny":
				if ev.Content.Bytes != 4 {
					t.Fatalf("%q small manifest: %+v", q, ev.Content)
				}
			default:
				if utf8.RuneCountInString(ev.Detail) != 2000 || !strings.HasPrefix(bigBody, ev.Detail) || ev.Content.State != "full" || len(ev.Content.Parts) != 5 {
					t.Fatalf("%q big event: %d chars %+v", q, utf8.RuneCountInString(ev.Detail), ev.Content)
				}
			}
		}
		if strings.Contains(r.Body.String(), rawFull.original) || strings.Contains(r.Body.String(), `"hi"`) || strings.Contains(r.Body.String(), "a@x.example") {
			t.Fatalf("%q: list must not carry part contents", q)
		}
	}
	// 忽略的响应是同一口径，不再回正文全文。
	r := e.do(e.token(auth.Delegate), "POST", "/v1/source-events/"+id+"/ignore", ``)
	var ig struct {
		Detail  string          `json:"detail"`
		Content runner.Manifest `json:"content"`
	}
	r.into(&ig)
	if r.Code != 200 || utf8.RuneCountInString(ig.Detail) != 2000 || len(ig.Content.Parts) != 5 {
		t.Fatalf("ignore response: %d %d chars", r.Code, utf8.RuneCountInString(ig.Detail))
	}
}

// ACT-S8-03：升级前落盘的事件在单条读取里照常返回原列内容，状态 legacy；存储按原列长度现算。
func TestLegacyEventReadsOriginalColumnsAndIsCountedByStorage(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_old")
	old := strings.Repeat("旧邮件正文", 1000) // 5000 个字符，远超预览
	id := e.seedLegacy("src_old", "m1", old, `[{"sender":"bob","text":"hi","at":"2026-03-01T00:00:00Z"}]`)

	got := read(t, e.web("GET", "/v1/source-events/"+id, ""))
	if got.Content.State != "legacy" || got.Body == nil || sha(*got.Body) != sha(old) || got.Original != nil || got.Headers != nil {
		t.Fatalf("legacy read: state=%s %+v", got.Content.State, got.Content.Parts)
	}
	if utf8.RuneCountInString(got.Event.Detail) != 2000 {
		t.Fatalf("legacy event row holds the full text, the view must still be a preview: %d", utf8.RuneCountInString(got.Event.Detail))
	}
	want := int64(len(old) + len(`[{"sender":"bob","text":"hi","at":"2026-03-01T00:00:00Z"}]`))
	if got.Content.Bytes != want {
		t.Fatalf("legacy manifest bytes %d, want %d", got.Content.Bytes, want)
	}
	var st storageOut
	r := e.do(e.token(auth.Observer), "GET", "/v1/storage", "")
	r.into(&st)
	if r.Code != 200 || len(st.Sources) != 1 || st.Sources[0].ContentBytes != want || st.ContentBytes != want {
		t.Fatalf("storage must count legacy events by their original columns: %d %s", r.Code, r.Body)
	}
}

// ACT-STO-01：存储用量与来源 stats 同源；事件数、原文字节、最早时间与逐条累加一致；账本与附件各自计字节。
func TestStorageReportsUsageAndSourceStatsAgree(t *testing.T) {
	e := setup(t)
	if err := os.WriteFile(filepath.Join(e.blobs, "aa"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.registerRaw("src_a", "src_b")
	a1 := e.seedRaw("src_a", "1", rawFull)
	a2 := e.seedRaw("src_a", "2", rawEvent{body: "short body"})
	e.seedRaw("capture:prj_1", "c", rawEvent{body: "conversation"})
	e.update( // 保留天数由 raw-retention-purge 的路由写；这里直接设列，只验存储读到它
		func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE sources_registry_source SET content_retention_days = 90 WHERE id = 'src_a'`)
			return err
		})

	var sum int64
	for _, id := range []string{a1, a2} {
		sum += read(t, e.web("GET", "/v1/source-events/"+id, "")).Content.Bytes
	}
	var st storageOut
	r := e.do(e.token(auth.Observer), "GET", "/v1/storage", "")
	r.into(&st)
	if r.Code != 200 || st.LedgerBytes <= 0 || st.BlobsBytes != 5 || len(st.Sources) != 2 {
		t.Fatalf("storage: %d %s", r.Code, r.Body)
	}
	a, b := st.Sources[0], st.Sources[1]
	if a.ID != "src_a" || a.Events != 2 || a.ContentBytes != sum || !a.Oldest.Equal(e.now) || a.RetentionDays == nil || *a.RetentionDays != 90 {
		t.Fatalf("src_a: %+v want events 2 bytes %d", a, sum)
	}
	if b.ID != "src_b" || b.Events != 0 || b.ContentBytes != 0 || !b.Oldest.IsZero() || b.RetentionDays != nil || b.Name == "" {
		t.Fatalf("src_b: %+v", b)
	}
	if st.ContentBytes != sum+int64(len("conversation")) {
		t.Fatalf("total content bytes %d counts every stored event", st.ContentBytes)
	}

	var one struct {
		Stats struct {
			Events       int64 `json:"events"`
			ContentBytes int64 `json:"content_bytes"`
			Runs         *int  `json:"runs"`
		} `json:"stats"`
	}
	e.do(e.token(auth.Observer), "GET", "/v1/sources/src_a", "").into(&one)
	if one.Stats.Events != 2 || one.Stats.ContentBytes != sum {
		t.Fatalf("source stats must agree with /storage: %+v", one.Stats)
	}
	var list struct {
		Sources []struct {
			ID    string `json:"id"`
			Stats struct {
				Events       *int64 `json:"events"`
				ContentBytes *int64 `json:"content_bytes"`
			} `json:"stats"`
		} `json:"sources"`
	}
	e.do(e.token(auth.Observer), "GET", "/v1/sources", "").into(&list)
	if len(list.Sources) != 2 || list.Sources[1].Stats.Events == nil || *list.Sources[1].Stats.Events != 0 || list.Sources[1].Stats.ContentBytes == nil {
		t.Fatalf("a source without events still reports zero counts: %+v", list.Sources)
	}
	if r := e.do(e.token(auth.Ext), "GET", "/v1/storage", ""); r.Code != 403 {
		t.Errorf("ext reading storage: %d", r.Code)
	}
}

// ACT-RAW-06：没有任何写来源原文的路由；改事项正文不影响来源事件原文。
func TestNoRouteWritesSourceContentAndItemBodyEditLeavesItAlone(t *testing.T) {
	e := setup(t)
	e.registerRaw("src_raw")
	id := e.seedRaw("src_raw", "k", rawFull)
	for _, m := range []string{"PUT", "PATCH", "POST", "DELETE"} {
		for _, path := range []string{"/v1/source-events/" + id, "/v1/source-events/" + id + "/content", "/v1/storage"} {
			if r := e.web(m, path, `{"body":"x"}`); r.Code != 404 && r.Code != 405 {
				t.Errorf("%s %s: %d (a route that writes source content must not exist)", m, path, r.Code)
			}
		}
	}
	before := read(t, e.web("GET", "/v1/source-events/"+id+"?part=body", ""))
	res, err := e.x.Do(bg, command.Request{Identity: webIdentity, Command: "item.create", Input: json.RawMessage(`{"kind":"task","title":"t","body":"orig"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var it struct{ ID string }
	_ = json.Unmarshal(res.Value, &it)
	if _, err := e.x.Do(bg, command.Request{Identity: webIdentity, Command: "item.update", Input: json.RawMessage(`{"id":"` + it.ID + `","body":"改过的正文"}`)}); err != nil {
		t.Fatal(err)
	}
	after := read(t, e.web("GET", "/v1/source-events/"+id+"?part=body", ""))
	if before.Body == nil || after.Body == nil || sha(*before.Body) != sha(*after.Body) || sha(*after.Body) != sha(bigBody) {
		t.Fatal("editing an item body changed the source event body")
	}
}
