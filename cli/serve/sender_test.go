package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	sregistry "github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 上限按仓库自带的接入声明（§9.1）：没有声明的程序一律未核实；只用映射的 webhook 以验签为准，带转换脚本的不算。
func TestSenderCeilingFollowsDeclarations(t *testing.T) {
	cases := []struct {
		src  domain.Source
		typ  string
		want runner.SenderCeiling
	}{
		{domain.Source{Type: domain.SourceScript, Command: "mail-himalaya"}, "mail.received", runner.CeilingUnverified},
		{domain.Source{Type: domain.SourceScript, Command: "/opt/my-feed.sh"}, "comment.created", runner.CeilingUnverified},
		{domain.Source{Type: domain.SourcePull, Command: "github-source"}, "issue.commented", runner.CeilingVerified},
		{domain.Source{Type: domain.SourcePull, Command: "github-source"}, "pr.closed", runner.CeilingNone},
		{domain.Source{Type: domain.SourceScript, Command: "apple-pim-source"}, "reminder.due", runner.CeilingNone},
		{domain.Source{Type: domain.SourceWebhook, SourceKind: "github"}, "pr.review_commented", runner.CeilingVerified},
		{domain.Source{Type: domain.SourceWebhook, SourceKind: "forge", Config: json.RawMessage(`{"mappings":["m"]}`)}, "x.y", runner.CeilingVerified},
		{domain.Source{Type: domain.SourceWebhook, SourceKind: "forge", Config: json.RawMessage(`{"transform":{"command":"/x"}}`)}, "x.y", runner.CeilingUnverified},
	}
	for _, c := range cases {
		if got := senderCeiling(c.src, c.typ); got != c.want {
			t.Errorf("%s %q %s %s: got %s, want %s", c.src.Type, c.src.Command, c.src.SourceKind, c.typ, got, c.want)
		}
	}
}

// T2、T3：经真实的 runner 落盘：需确认来源与没有声明的自定义脚本自报的核实被压成未核实；
// 声明为不带发件人的类型清空发件人；可信来源（拉取与 webhook）的评论保持已核实。
func TestSelfReportedSenderIsCappedByDeclarationOnStore(t *testing.T) {
	k := assembled(t)
	k.do(t, command.Chain{}, sregistry.CmdRegisterExec, map[string]any{"id": "src_mail", "type": "script", "source_type": "mail",
		"command": "mail-himalaya", "config": map[string]any{"account": "gmail"}, "schedule": "1h", "timeout": 10})
	k.do(t, command.Chain{}, sregistry.CmdRegisterExec, map[string]any{"id": "src_custom", "name": "巡检", "type": "script", "source_type": "acc",
		"command": "/bin/sh", "schedule": "1h", "timeout": 10})
	k.do(t, command.Chain{}, sregistry.CmdRegister, map[string]any{"id": "src_gh", "type": "pull", "source_type": "github",
		"command": "github-source", "config": map[string]any{"repos": []string{"a/b"}}, "schedule": "1h", "timeout": 10})
	k.do(t, command.Chain{}, sregistry.CmdRegister, map[string]any{"id": "src_ghw", "name": "推送", "type": "webhook", "source_type": "github",
		"config": map[string]any{"scheme": "hmac-sha256-header", "mappings": []string{"github-issue-comments"}}})

	ev := func(id, typ string) string {
		b, _ := json.Marshal(map[string]any{"id": id, "type": typ, "subject": id, "occurred_at": "2026-01-01T00:00:00Z",
			"sender": "alice", "sender_verified": true})
		return string(b)
	}
	pull := func(src string, lines ...string) {
		code := 0
		res := proc.Result{Run: domain.Run{ID: "run_" + src, Category: domain.RunSource, Target: src, State: domain.RunSucceeded, ExitCode: &code},
			Stdout: []byte(strings.Join(lines, "\n") + "\n{\"cursor\":\"c\"}\n")}
		if err := k.rn.Handle(context.Background(), res); err != nil {
			t.Fatal(err)
		}
	}
	pull("src_mail", ev("m1", "mail.received"))
	pull("src_custom", ev("c1", "comment.created"))
	pull("src_gh", ev("g1", "issue.commented"), ev("g2", "issue.closed"))
	in, _ := json.Marshal(map[string]any{"source_id": "src_ghw", "run_id": "ing_1", "events": []json.RawMessage{json.RawMessage(ev("w1", "issue.commented"))}})
	if _, err := k.x.Do(context.Background(), command.Request{Identity: runner.Internal, Command: runner.CmdIngest, Input: in}); err != nil {
		t.Fatal(err)
	}

	got := map[string]domain.SourceEvent{}
	for _, src := range []string{"src_mail", "src_custom", "src_gh", "src_ghw"} {
		var evs []domain.SourceEvent
		if err := k.st.View(context.Background(), func(tx *store.Tx) (err error) { evs, err = runner.Events(tx, src); return }); err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			got[e.DedupeKey.EventID] = e
		}
	}
	want := map[string]struct {
		sender   string
		verified bool
	}{"m1": {"alice", false}, "c1": {"alice", false}, "g1": {"alice", true}, "g2": {"", false}, "w1": {"alice", true}}
	for id, w := range want {
		if e, ok := got[id]; !ok || e.Sender != w.sender || e.SenderVerified != w.verified {
			t.Errorf("%s: got %+v, want sender %q verified %v", id, got[id], w.sender, w.verified)
		}
	}
}

// identity 模式（§9.2）：经外呼调用器运行「程序名 identity」，stdin 与拉取相同；输出末行的 sender 交回，失败报错。
func TestIdentityModeRunsProgramAndReturnsSender(t *testing.T) {
	k := assembled(t)
	bin := t.TempDir()
	stdinLog := filepath.Join(bin, "stdin.log")
	prog := filepath.Join(bin, "github-source")
	script := "#!/bin/sh\ncat > " + stdinLog + "\n[ \"$1\" = identity ] || exit 9\n[ -e " + filepath.Join(bin, "fail") + " ] && exit 2\necho '{\"sender\":\"octo\"}'\n"
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	k.do(t, command.Chain{}, sregistry.CmdRegisterExec, map[string]any{"id": "src_gh", "type": "script", "source_type": "github",
		"command": prog + " --ignored", "config": map[string]any{"repos": []string{"a/b"}}, "schedule": "1h", "timeout": 10})
	var src domain.Source
	if err := k.st.View(context.Background(), func(tx *store.Tx) (err error) { src, err = sregistry.Get(tx, "src_gh"); return }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	who, err := k.ids.Fetch(ctx, src)
	if err != nil || who != "octo" {
		t.Fatalf("identity: %q %v", who, err)
	}
	b, _ := os.ReadFile(stdinLog)
	var in struct {
		SourceID string          `json:"source_id"`
		Config   json.RawMessage `json:"config"`
	}
	if json.Unmarshal(b, &in) != nil || in.SourceID != "src_gh" || string(in.Config) != `{"repos":["a/b"]}` {
		t.Fatalf("stdin: %s", b)
	}
	if err := os.WriteFile(filepath.Join(bin, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var de *domain.Error
	if _, err := k.ids.Fetch(ctx, src); !errors.As(err, &de) || de.Code != domain.CodeConflict {
		t.Fatalf("failed identity run: %v", err)
	}
	var evs []domain.SourceEvent
	if err := k.st.View(context.Background(), func(tx *store.Tx) (err error) { evs, err = runner.Events(tx, "src_gh"); return }); err != nil || len(evs) != 0 {
		t.Fatalf("identity runs must not store events: %v %v", evs, err)
	}
}
