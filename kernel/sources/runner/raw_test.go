package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (e *env) manifest(id string) (mf Manifest) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { mf, err = ContentOf(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) part(id, part string) (p PartData, err error) {
	e.t.Helper()
	err = e.st.View(bg, func(tx *store.Tx) (err error) { p, err = ReadPart(tx, id, part); return })
	return
}

func (e *env) previewColumn(id string) (s string) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT detail FROM sources_runner_event WHERE id = ?`, id)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			return r.Scan(&s)
		}
		return r.Err()
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func pull(e *env, id, lines string) {
	e.t.Helper()
	e.register(id, e.script(id+".sh", lines), nil)
	e.r.Sync(bg)
	e.tick(1)
}

// ACT-RAW-01：正文、原始正文、头部、附件清单、聚合消息各存一个部分（媒体类型、字节数、sha256），
// 事件行只留前 2000 个字符的预览，读取拿到全文；content.state 为 full。
func TestAllContentFieldsLandAsPartsAndRowKeepsOnlyPreview(t *testing.T) {
	e := setup(t, defaults())
	body := strings.Repeat("长正文", 1500) // 4500 个字符，远超预览
	html := "<p>" + strings.Repeat("x", 30000) + "</p>"
	pull(e, "src_a", `cat <<'EOF'
{"id":"k1","type":"mail.received","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"`+body+`","original":{"media_type":"text/html","text":"`+html+`"},"headers":{"From":"a@x.example","Received":["r1","r2"]},"attachments":[{"name":"a.pdf","media_type":"application/pdf","size":2048,"ref":"h1"}],"items":[{"sender":"bob","text":"hello","at":"2026-03-01T00:00:00Z"}]}
{"cursor":"c1"}
EOF
`)
	evs := e.events("src_a")
	if len(evs) != 1 || evs[0].Detail != body || len(evs[0].Messages) != 1 {
		t.Fatalf("read-back lost content: %d events", len(evs))
	}
	id := evs[0].ID
	if got := e.previewColumn(id); utf8.RuneCountInString(got) != 2000 || !strings.HasPrefix(body, got) {
		t.Fatalf("row keeps %d chars, want a 2000-char preview", utf8.RuneCountInString(got))
	}
	mf := e.manifest(id)
	if mf.State != "full" || len(mf.Parts) != 5 || len(mf.Missing) != 0 {
		t.Fatalf("manifest: %+v", mf)
	}
	var total int64
	for _, p := range mf.Parts {
		total += p.Bytes
		got, err := e.part(id, p.Part)
		if err != nil || int64(len(got.Content)) != p.Bytes || sum(string(got.Content)) != p.SHA256 || got.MediaType != p.MediaType {
			t.Fatalf("part %s: %+v err=%v", p.Part, p, err)
		}
	}
	if total != mf.Bytes {
		t.Fatalf("total %d != sum of parts %d", mf.Bytes, total)
	}
	if o, _ := e.part(id, "original"); o.MediaType != "text/html" || string(o.Content) != html {
		t.Fatalf("original: %s %d bytes", o.MediaType, len(o.Content))
	}
	if b, _ := e.part(id, "body"); string(b.Content) != body || b.Bytes != int64(len(body)) {
		t.Fatalf("body part truncated: %d bytes", len(b.Content))
	}
}

// ACT-RAW-02：来源程序自报 partial 时，清单记 partial 与缺哪一部分、为什么；没带原文字段的事件按 full 记，也只有 body 一个部分。
func TestPartialSelfReportAndPlainEventState(t *testing.T) {
	e := setup(t, defaults())
	pull(e, "src_a", `echo '{"id":"p1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"half","content":{"state":"partial","missing":[{"part":"attachments","reason":"too big"}]}}'
echo '{"id":"p2","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"all"}'
echo '{"cursor":"c1"}'
`)
	byKey := map[string]domain.SourceEvent{}
	for _, ev := range e.events("src_a") {
		byKey[ev.DedupeKey.EventID] = ev
	}
	p := e.manifest(byKey["p1"].ID)
	if p.State != "partial" || len(p.Missing) != 1 || p.Missing[0].Part != "attachments" || p.Missing[0].Reason != "too big" {
		t.Fatalf("partial manifest: %+v", p)
	}
	if f := e.manifest(byKey["p2"].ID); f.State != "full" || len(f.Parts) != 1 || f.Parts[0].Part != "body" || f.Parts[0].MediaType == "" {
		t.Fatalf("plain manifest: %+v", f)
	}
}

func TestInvalidContentFieldsAreRejected(t *testing.T) {
	base := `{"id":"x","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z",`
	for name, tc := range map[string]struct{ line, want string }{
		"state unknown":      {base + `"content":{"state":"legacy"}}`, "content.state"},
		"partial no missing": {base + `"content":{"state":"partial"}}`, "content.missing"},
		"missing bad part":   {base + `"content":{"state":"partial","missing":[{"part":"nope","reason":"r"}]}}`, "content.missing"},
		"header not string":  {base + `"headers":{"A":1}}`, "headers.A"},
		"attachment no name": {base + `"attachments":[{"size":1}]}`, "attachments[0].name"},
		"original no type":   {base + `"original":{"text":"x"}}`, "original.media_type"},
		"msg attachment no kind": {base + `"items":[{"sender":"a","text":"","at":"2026-03-01T00:00:00Z","attachments":[{"name":"a.png"}]}]}`,
			"items[0].attachments[0].kind"},
	} {
		p := parse([]byte(tc.line+"\n"), 1<<20)
		if len(p.events) != 0 || len(p.rejects) != 1 || !strings.Contains(p.rejects[0].Reason, tc.want) {
			t.Errorf("%s: events=%d rejects=%+v", name, len(p.events), p.rejects)
		}
	}
}

// 只有附件、没有文字的消息（图片、文件、语音）照常入账，不连累同一事件里的其他消息；
// 原文的 messages 部分与读回的事件都带着附件的类型、名字与媒体类型，送给模型的消息行写明这是一条什么消息。
// 契约改回「items[].text 非空」，这条事件会被整条拒收，本用例失败。
func TestAttachmentOnlyMessagesLandWithAttachmentMetadata(t *testing.T) {
	e := setup(t, defaults())
	pull(e, "src_a", `cat <<'EOF'
{"id":"w1","type":"chat.window","subject":"g","occurred_at":"2026-03-01T00:03:00Z","items":[{"sender":"bob","text":"看下这个","at":"2026-03-01T00:00:00Z"},{"sender":"bob","text":"","at":"2026-03-01T00:01:00Z","attachments":[{"kind":"file","name":"周报.pdf","media_type":"application/pdf","ref":"h1"}]},{"sender":"bob","text":"","at":"2026-03-01T00:02:00Z","attachments":[{"kind":"image"}]},{"sender":"bob","text":"","at":"2026-03-01T00:03:00Z"}]}
{"cursor":"c1"}
EOF
`)
	if rj := e.rejects("src_a"); len(rj) != 0 {
		t.Fatalf("attachment-only messages rejected: %+v", rj)
	}
	evs := e.events("src_a")
	if len(evs) != 1 || len(evs[0].Messages) != 4 {
		t.Fatalf("events=%d, want 1 with 4 messages", len(evs))
	}
	m := evs[0].Messages
	if a := m[1].Attachments; len(a) != 1 || a[0].Kind != "file" || a[0].Name != "周报.pdf" || a[0].MediaType != "application/pdf" || a[0].Ref != "h1" {
		t.Fatalf("file attachment not read back: %+v", m[1])
	}
	if p, err := e.part(evs[0].ID, "messages"); err != nil || !strings.Contains(string(p.Content), `"name":"周报.pdf"`) || !strings.Contains(string(p.Content), `"kind":"image"`) {
		t.Fatalf("messages part lost attachments: %s %v", p.Content, err)
	}
	for i, want := range []string{"看下这个", `[attachment kind=file name="周报.pdf" media_type=application/pdf]`, "[attachment kind=image]", "[no text, no attachment]"} {
		if got := m[i].Body(); got != want {
			t.Errorf("message %d body = %q, want %q", i, got, want)
		}
	}
}

// ACT-RAW-03：升级前落盘的事件（没有原文部分）读取先看原文表、没有再回退旧列：正文与聚合消息照旧可读，状态是 legacy，其余部分不存在。
func TestLegacyEventReadsFallBackToOldColumns(t *testing.T) {
	e := setup(t, defaults())
	pull(e, "src_a", `echo '{"id":"n1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'
echo '{"cursor":"c1"}'
`)
	id := e.events("src_a")[0].ID
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM sources_runner_event_part WHERE event_id = ?`, id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE sources_runner_event SET detail = 'old body', messages = '[{"sender":"a","text":"m","at":"2026-03-01T00:00:00Z"}]',
			content_bytes = NULL, content_state = 'legacy' WHERE id = ?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ev := e.events("src_a")[0]
	if ev.Detail != "old body" || len(ev.Messages) != 1 {
		t.Fatalf("legacy columns not read: %+v", ev)
	}
	mf := e.manifest(id)
	if mf.State != "legacy" || len(mf.Parts) != 2 || mf.Bytes != mf.Parts[0].Bytes+mf.Parts[1].Bytes || mf.Parts[0].Bytes != int64(len("old body")) {
		t.Fatalf("legacy manifest: %+v", mf)
	}
	if b, err := e.part(id, "body"); err != nil || string(b.Content) != "old body" || b.SHA256 != sum("old body") {
		t.Fatalf("legacy body: %+v %v", b, err)
	}
	if _, err := e.part(id, "original"); err == nil {
		t.Fatal("legacy event has no original part")
	}
}

// ACT-RLIM-02：单条事件超过上限整行拒收，原因 event_too_large 带实际字节数；其余事件照常落盘，游标照常推进；被拒的事件不留半条。
func TestEventOverLimitRejectedWholeOthersLandCursorAdvances(t *testing.T) {
	e := setup(t, defaults())
	e.r.o.MaxEvent = 4 << 10
	big := strings.Repeat("a", 3<<10)
	pull(e, "src_a", `echo '{"id":"ok","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"small"}'
echo '{"id":"big","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"`+big+`","original":{"media_type":"text/plain","text":"`+big+`"}}'
echo '{"id":"ok2","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'
echo '{"cursor":"c1"}'
`)
	s, st := e.source("src_a")
	if s.Cursor != "c1" || st.ConsecutiveFailures != 0 || len(e.events("src_a")) != 2 {
		t.Fatalf("cursor=%q stats=%+v events=%d", s.Cursor, st, len(e.events("src_a")))
	}
	rj := e.rejects("src_a")
	if len(rj) != 1 || rj[0].Line != 2 || !strings.HasPrefix(rj[0].Reason, "event_too_large") || !strings.Contains(rj[0].Reason, "6144 bytes") {
		t.Fatalf("rejects: %+v", rj)
	}
	if n := e.count(`SELECT COUNT(*) FROM sources_runner_event WHERE event_id = 'big'`); n != 0 {
		t.Fatal("rejected event left a row behind")
	}
}

// ACT-RLIM-03：聚合消息超过 200 条整行拒收，原因写明条数，游标照常推进。
func TestTooManyMessagesRejectedCursorAdvances(t *testing.T) {
	e := setup(t, defaults())
	pull(e, "src_a", `printf '{"id":"m1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","items":['
i=0; while [ $i -lt 201 ]; do [ $i -gt 0 ] && printf ','; printf '{"sender":"a","text":"t","at":"2026-03-01T00:00:00Z"}'; i=$((i+1)); done
echo ']}'
echo '{"id":"m2","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'
echo '{"cursor":"c1"}'
`)
	s, _ := e.source("src_a")
	rj := e.rejects("src_a")
	if s.Cursor != "c1" || len(e.events("src_a")) != 1 || len(rj) != 1 || !strings.Contains(rj[0].Reason, "201") {
		t.Fatalf("cursor=%q events=%d rejects=%+v", s.Cursor, len(e.events("src_a")), rj)
	}
}

// ACT-RLIM-04：stdout 超过总上限时，上限内的事件连同原文各部分照常落盘，游标不动，按临时失败计；重试不改写已存的原文。
func TestOutputOverLimitKeepsPartsHoldsCursorAndRetryDoesNotRewrite(t *testing.T) {
	cfg := defaults()
	cfg.MaxSourceOutput = 32 << 10
	e := setup(t, cfg)
	e.register("src_a", e.script("a.sh", `i=0; while [ $i -lt 700 ]; do
echo '{"id":"e'$i'","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"body '$i'","original":{"media_type":"text/plain","text":"orig '$i'"}}'
i=$((i+1)); done
echo '{"cursor":"big"}'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	s, st := e.source("src_a")
	evs := e.events("src_a")
	if s.Cursor != "" || st.ConsecutiveFailures != 1 || len(evs) == 0 || len(evs) >= 700 {
		t.Fatalf("cursor=%q stats=%+v events=%d", s.Cursor, st, len(evs))
	}
	if o, err := e.part(evs[0].ID, "original"); err != nil || !strings.HasPrefix(string(o.Content), "orig ") {
		t.Fatalf("original part of a kept event: %+v %v", o, err)
	}
	before := len(evs)
	e.advance(time.Minute)
	e.tick(1)
	if got := len(e.events("src_a")); got != before {
		t.Fatalf("retry duplicated events: %d -> %d", before, got)
	}
}

// 任何入口都不能改写来源原文：同一事件再次到达（去重）时，已存的各部分与预览不变。
func TestDuplicateEventNeverRewritesStoredOriginal(t *testing.T) {
	e := setup(t, defaults())
	pull(e, "src_a", `echo '{"id":"d1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"first","original":{"media_type":"text/plain","text":"first-orig"}}'
echo '{"id":"d1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z","detail":"second","original":{"media_type":"text/plain","text":"second-orig"}}'
echo '{"cursor":"c1"}'
`)
	evs := e.events("src_a")
	if len(evs) != 1 || evs[0].Detail != "first" {
		t.Fatalf("events: %+v", evs)
	}
	if o, _ := e.part(evs[0].ID, "original"); string(o.Content) != "first-orig" {
		t.Fatalf("original rewritten: %q", o.Content)
	}
	if n := e.count(`SELECT COUNT(*) FROM sources_runner_event_part`); n != 2 {
		t.Fatalf("parts = %d, want body+original of the first event only", n)
	}
}
