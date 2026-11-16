package srcstub

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var exe string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "srcstub")
	exe = filepath.Join(dir, "src-stub")
	if out, err := exec.Command("go", "build", "-o", exe, "./cmd/src-stub").CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run 进程内调用 Run（覆盖率统计在内）；binDir 为空目录，env 为全部环境变量。
func run(t *testing.T, stdin string, env []string, args ...string) (stdout []byte, stderr string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	code = Run(args, env, t.TempDir(), strings.NewReader(stdin), &o, &e)
	return o.Bytes(), e.String(), code
}

// TestBinary 以真实子进程运行构建出的命令，确认入口接线（环境变量、stdin、退出码）。
func TestBinary(t *testing.T) {
	cmd := exec.Command(exe, "fetch")
	cmd.Env = []string{"SRC_FETCH=fail:2"}
	cmd.Stdin = strings.NewReader(fetchIn)
	var e bytes.Buffer
	cmd.Stderr = &e
	if ee, ok := cmd.Run().(*exec.ExitError); !ok || ee.ExitCode() != 2 || e.Len() == 0 {
		t.Fatalf("binary fetch fail: %v %q", ee, e.String())
	}
	cmd = exec.Command(exe)
	cmd.Env = []string{"SRC_CASE=mid-body"}
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), `"cursor"`) {
		t.Fatalf("binary pull: %v", err)
	}
}

type event struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Subject     string         `json:"subject"`
	OccurredAt  string         `json:"occurred_at"`
	Detail      string         `json:"detail"`
	Headers     map[string]any `json:"headers"`
	Attachments []struct {
		Name, MediaType string
		Size            int64
		Ref             string
	} `json:"attachments"`
	Items    []struct{ Sender, Text, At string } `json:"items"`
	Original *struct {
		MediaType string `json:"media_type"`
		Text      string
	} `json:"original"`
	Content *struct {
		State   string
		Missing []struct{ Part, Reason string }
	} `json:"content"`
	Cursor string `json:"cursor"`
}

func pull(t *testing.T, c string, extra ...string) (evs []event, cursor string) {
	t.Helper()
	out, stderr, code := run(t, `{"source_id":"s","cursor":"","window":{}}`, append([]string{"SRC_CASE=" + c}, extra...))
	if code != 0 {
		t.Fatalf("case %s exit %d: %s", c, code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, l := range lines {
		var e event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("case %s line %d not JSON: %v", c, i, err)
		}
		if i == len(lines)-1 {
			return evs, e.Cursor
		}
		if e.ID == "" || e.Type == "" || e.Subject == "" || e.OccurredAt == "" {
			t.Fatalf("case %s line %d lacks required field: %s", c, i, l)
		}
		evs = append(evs, e)
	}
	return
}

func TestBigBodyIs200KiBStable(t *testing.T) {
	a, cur := pull(t, "big-body")
	b, _ := pull(t, "big-body")
	if len(a) != 1 || len(a[0].Detail) != 204800 || a[0].Detail != b[0].Detail || a[0].ID != b[0].ID || cur == "" {
		t.Fatalf("big-body not 204800 stable bytes (len=%d cursor=%q)", len(a[0].Detail), cur)
	}
	r, _ := pull(t, "big-body-reply")
	u, _ := pull(t, "big-body-update")
	if r[0].ID == a[0].ID || len(u[0].Detail) != 204800 || u[0].Detail[:200] == a[0].Detail[:200] {
		t.Fatal("reply/update must be distinct events with 200 KiB body and changed head")
	}
}

func TestMailLikeCarriesHeadersAttachmentsOriginal(t *testing.T) {
	evs, _ := pull(t, "mail-like", "SRC_IMG_URL=http://rec.invalid/p.png")
	e := evs[0]
	if _, ok := e.Headers["To"].([]any); !ok || e.Headers["Message-ID"] == nil {
		t.Fatalf("headers: %v", e.Headers)
	}
	if len(e.Attachments) != 2 || e.Attachments[0].Ref == "" || e.Attachments[1].Ref == "" || e.Attachments[0].Name == "" || e.Attachments[0].Size <= 0 {
		t.Fatalf("attachments: %+v", e.Attachments)
	}
	if e.Original == nil || e.Original.MediaType != "text/html" || !strings.Contains(e.Original.Text, "<script>") || !strings.Contains(e.Original.Text, "http://rec.invalid/p.png") {
		t.Fatalf("original: %+v", e.Original)
	}
}

func TestMessagesCases(t *testing.T) {
	for c, n := range map[string]int{"chat-two-things": 4, "chat-long": 60, "too-many-msgs": 201} {
		evs, _ := pull(t, c)
		if len(evs) != 1 || len(evs[0].Items) != n {
			t.Fatalf("%s: want %d items", c, n)
		}
		for i, it := range evs[0].Items {
			if it.Sender == "" || it.Text == "" || it.At == "" {
				t.Fatalf("%s item %d incomplete: %+v", c, i, it)
			}
			if c == "chat-long" && len(it.Text) != 100 {
				t.Fatalf("chat-long item %d len %d", i, len(it.Text))
			}
		}
	}
}

func TestOversizeMidSitsBetweenTwoNormalEvents(t *testing.T) {
	evs, _ := pull(t, "oversize-mid")
	if len(evs) != 3 || len(evs[0].Detail) > 100 || len(evs[2].Detail) > 100 || len(evs[1].Detail) != OversizeBytes {
		t.Fatalf("oversize-mid shape wrong: %d events", len(evs))
	}
	evs, _ = pull(t, "oversize-mid", "SRC_OVERSIZE_BYTES=2097152")
	if len(evs[1].Detail) != 2097152 {
		t.Fatal("SRC_OVERSIZE_BYTES ignored")
	}
}

func TestPartialSelfReport(t *testing.T) {
	evs, _ := pull(t, "partial")
	c := evs[0].Content
	if c == nil || c.State != "partial" || len(c.Missing) != 2 || c.Missing[0].Part != "original" || c.Missing[1].Part != "attachments" || c.Missing[0].Reason == "" {
		t.Fatalf("content: %+v", c)
	}
}

func TestBodySizes(t *testing.T) {
	if evs, _ := pull(t, "mid-body"); len(evs[0].Detail) != 3000 {
		t.Fatal("mid-body not 3000 chars")
	}
	evs, _ := pull(t, "digest-30k")
	if len(evs) != 2 || len(evs[0].Detail) != 30000 || len(evs[1].Detail) != 30000 {
		t.Fatal("digest-30k wrong")
	}
	if evs, _ := pull(t, "ten-big"); len(evs) != 10 || len(evs[9].Detail) != 204800 {
		t.Fatal("ten-big wrong")
	}
}

func TestCaseSourcesAndFailures(t *testing.T) {
	if _, _, code := run(t, "{}", nil); code != ExitNoCase {
		t.Fatalf("no case => %d", code)
	}
	if _, stderr, code := run(t, "{}", []string{"SRC_CASE=nope"}); code != ExitBadArgs || !strings.Contains(stderr, "nope") {
		t.Fatalf("unknown case => %d %q", code, stderr)
	}
	dir := t.TempDir()
	// case 文件里是 case 名
	f := filepath.Join(dir, "c")
	os.WriteFile(f, []byte("mid-body\n"), 0o644)
	if evs, _ := pull(t, f); len(evs) != 1 || evs[0].ID != "mid-body-1" {
		t.Fatal("case file name not honored")
	}
	// 执行者追加的原样 JSONL，缺游标时补一个
	os.WriteFile(f, []byte(`{"id":"x1","type":"t","subject":"s","occurred_at":"2026-11-10T09:00:00Z"}`), 0o644)
	if evs, cur := pull(t, f); len(evs) != 1 || evs[0].ID != "x1" || cur != "custom" {
		t.Fatalf("custom jsonl: %v %q", evs, cur)
	}
	// 与可执行文件同目录的 case 文件（环境变量被白名单拦掉时用）
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "case"), []byte("mid-body\n"), 0o644)
	var o, e bytes.Buffer
	if code := Run(nil, nil, bin, strings.NewReader(""), &o, &e); code != 0 || !strings.Contains(o.String(), "mid-body-1") {
		t.Fatalf("beside-binary case: %d %s", code, e.String())
	}
	os.WriteFile(filepath.Join(bin, "fetch"), []byte("fail"), 0o644)
	if code := Run([]string{"fetch"}, nil, bin, strings.NewReader(fetchIn), &o, &e); code != 1 {
		t.Fatalf("beside-binary fetch mode: %d", code)
	}
	if _, _, code := run(t, "", []string{"SRC_CASE=mid-body"}, "bogus"); code != ExitBadArgs {
		t.Fatal("unknown arg accepted")
	}
}

const fetchIn = `{"source_id":"s","event_id":"mail-like-1","attachment":{"ref":"att-0","name":"report.pdf"}}`

func TestFetchReturnsFixedBytesByRef(t *testing.T) {
	out, _, code := run(t, fetchIn, nil, "fetch")
	if code != 0 || len(out) != FetchFixedSize || SHA256(out) != SHA256(FetchBytes("att-0")) {
		t.Fatalf("fetch att-0 code=%d len=%d", code, len(out))
	}
	out2, _, _ := run(t, strings.Replace(fetchIn, "att-0", "att-1", 1), nil, "fetch")
	if bytes.Equal(out, out2) {
		t.Fatal("different refs must give different bytes")
	}
	// ref 来自 stdin，不是 case
	evs, _ := pull(t, "mail-like")
	if SHA256(FetchBytes(evs[0].Attachments[0].Ref)) != SHA256(out) {
		t.Fatal("listed ref does not fetch to the same bytes")
	}
}

func TestFetchConfiguredFailureAndOversize(t *testing.T) {
	_, stderr, code := run(t, fetchIn, []string{"SRC_FETCH=fail"}, "fetch")
	if code != 1 || strings.Count(strings.TrimSpace(stderr), "\n") != 0 || stderr == "" {
		t.Fatalf("fail => %d %q", code, stderr)
	}
	if _, _, code := run(t, fetchIn, []string{"SRC_FETCH=fail:3"}, "fetch"); code != 3 {
		t.Fatalf("fail:3 => %d", code)
	}
	out, _, code := run(t, fetchIn, []string{"SRC_FETCH=big"}, "fetch")
	if code != 0 || len(out) != 33<<20 || len(out) <= 32<<20 {
		t.Fatalf("big => %d len %d", code, len(out))
	}
	if out, _, _ := run(t, fetchIn, []string{"SRC_FETCH=size:10"}, "fetch"); len(out) != 10 {
		t.Fatal("size:10")
	}
	if _, _, code := run(t, fetchIn, []string{"SRC_FETCH=weird"}, "fetch"); code != ExitBadArgs {
		t.Fatal("bad mode accepted")
	}
	if _, _, code := run(t, `{"attachment":{}}`, nil, "fetch"); code != ExitBadArgs {
		t.Fatal("missing ref accepted")
	}
}

func TestDeclarations(t *testing.T) {
	read := func(f string) map[string]any {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err := yaml.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	with, without := read("source.yaml"), read("source-nofetch.yaml")
	c, _ := with["content"].(map[string]any)
	if with["program"] != "src-stub" || c["attachments"] != "fetch" {
		t.Fatalf("source.yaml: %v", with)
	}
	if _, has := without["content"]; has || without["program"] != "src-stub" {
		t.Fatalf("source-nofetch.yaml must have no content section: %v", without)
	}
}
