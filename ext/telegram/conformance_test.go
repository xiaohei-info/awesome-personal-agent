package telegram

import (
	"context"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	cliext "github.com/xiaohei-info/awesome-personal-agent/cli/ext"
)

// 测试二进制兼任被测 ext：APA_TEST_EXT_MODE=good 走真实的 Main；=bad 是故意不做发送幂等的版本（记不住已发的消息）。
func TestMain(m *testing.M) {
	if mode := os.Getenv("APA_TEST_EXT_MODE"); mode != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		var st Store
		if mode == "bad" {
			st = &amnesiac{&mem{recs: map[string]Record{}}}
		}
		if err := Main(ctx, os.Getenv, os.Stderr, st); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// amnesiac 能按消息找投递（回答要用），但永远说"没发过"——即没有以投递 id 为幂等键。
type amnesiac struct{ *mem }

func (amnesiac) Lookup(string) (Record, bool) { return Record{}, false }

func runCheck(t *testing.T, mode string) cliext.Report {
	t.Setenv("APA_TEST_EXT_MODE", mode)
	return cliext.Check(context.Background(), cliext.Info{Name: "telegram-" + mode, Kind: cliext.KindChannel, Command: []string{os.Args[0]}},
		30*time.Second, io.Discard)
}

// ACT-CHP-11：apa ext check 对 Telegram ext 覆盖领取、幂等、回报、版本冲突、回答，全部通过。
func TestExtCheckPassesForTelegramExt(t *testing.T) {
	r := runCheck(t, "good")
	want := []string{"claim", "ack", "idempotent", "stale-version", "answer"}
	var got []string
	for _, c := range r.Checks {
		got = append(got, c.Name)
		if !c.OK {
			t.Errorf("check %s failed: %s", c.Name, c.Detail)
		}
	}
	if !r.OK || !slices.Equal(got, want) {
		t.Fatalf("report = %+v", r)
	}
}

// ACT-CHP-11：故意不做幂等的 ext 被指出幂等失败（证明自测不是对谁都放行）。
func TestExtCheckCatchesExtWithoutIdempotency(t *testing.T) {
	r := runCheck(t, "bad")
	if r.OK {
		t.Fatal("an ext that re-sends on a re-claim must not pass")
	}
	for _, c := range r.Checks {
		if c.Name == "idempotent" && !c.OK {
			return
		}
	}
	t.Fatalf("idempotent check should fail: %+v", r.Checks)
}

// ACT-TG-05、ACT-DPL-03：ext 不引用内核包或任何 harness 代码，也不监听端口（所有连接都是出站）。
func TestExtIsIndependentAndOutboundOnly(t *testing.T) {
	const self = "github.com/xiaohei-info/awesome-personal-agent/ext/telegram"
	var files []string
	filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if len(files) < 5 {
		t.Fatalf("source scan found only %v", files)
	}
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, ".") && !strings.HasPrefix(p, "github.com/go-telegram/bot") && !strings.HasPrefix(p, self) {
				t.Errorf("%s imports %s: ext may only use the Bot SDK and its own packages", f, p)
			}
		}
		src, _ := os.ReadFile(f)
		for _, banned := range []string{"StartWebhook", "ListenAndServe", "http.Server"} {
			if f != filepath.Join("fakebot", "fakebot.go") && strings.Contains(string(src), banned) {
				t.Errorf("%s uses %s: the ext must not accept inbound connections", f, banned)
			}
		}
	}
}
