package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
)

const agmRepo = "acme/widgets"

// fakeReader 在 PATH 上放一个 hosted-reader：stdout 与退出码由参数决定，并把收到的 stdin 记到文件。
func fakeReader(t *testing.T, stdout string, exit int) (stdinLog string) {
	t.Helper()
	bin := t.TempDir()
	stdinLog = filepath.Join(bin, "stdin.log")
	script := "#!/bin/sh\ncat > " + stdinLog + "\nprintf '%s\\n' '" + stdout + "'\nexit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "hosted-reader"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stdinLog
}

// changedEvent 让来源 Run 以成功终态交回一条 repo.agents_md_changed，走真实的 runner → 账本 → 提交后回调。
func (k *kernel) changedEvent(t *testing.T, id string) {
	t.Helper()
	ev, _ := json.Marshal(map[string]any{"id": id, "type": agentsMDChanged, "subject": agmRepo + " AGENTS.md",
		"occurred_at": "2026-01-01T00:00:00Z", "correlation_keys": []string{agmRepo}})
	code := 0
	res := proc.Result{
		Run:    domain.Run{ID: "run_" + id, Category: domain.RunSource, Target: "src_gh", State: domain.RunSucceeded, ExitCode: &code},
		Stdout: []byte(string(ev) + "\n{\"cursor\":\"c\"}\n"),
	}
	if err := k.rn.Handle(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	k.agmWG.Wait()
}

func agentsKernel(t *testing.T) *kernel { return agentsKernelFor(t, agmRepo) }

func agentsKernelFor(t *testing.T, remote string) *kernel {
	k := assembled(t)
	k.do(t, command.Chain{}, "source.register_script", map[string]any{"id": "src_gh", "type": "script", "source_type": "repo",
		"command": "/bin/true", "schedule": "1h", "timeout": 10, "dedupe_domain": "gh"})
	var p struct{ ID string }
	_ = json.Unmarshal(k.do(t, command.Chain{}, "project.create", map[string]any{"name": "widgets"}), &p)
	k.do(t, command.Chain{}, "project.repo.add", map[string]any{"id": p.ID, "remote": remote})
	return k
}

// 默认分支上的手写 AGENTS.md：事件到达后内核经只读读取器读到它，并做与 apa agents check 同一检测（漂移）。
func TestAgentsMDChangedEventRunsSameCheckAsAgentsCheck(t *testing.T) {
	stdinLog := fakeReader(t, `{"commit":"abc123","branch":"main","path":"AGENTS.md","origin":"remote","content":"hand written\n"}`, 0)
	k := agentsKernel(t)
	k.changedEvent(t, "e1")

	v, err := k.agm.Get(context.Background(), agmRepo)
	if err != nil || v.LastCheck == nil || v.LastCheck.State != "drift" || v.LastCheck.Host != "default-branch" {
		t.Fatalf("event must produce a default-branch drift check: %+v %v", v, err)
	}
	if in, _ := os.ReadFile(stdinLog); !strings.Contains(string(in), `"repo":"`+agmRepo+`"`) || !strings.Contains(string(in), `"path":"AGENTS.md"`) {
		t.Fatalf("reader was not asked for the repo's AGENTS.md: %s", in)
	}
}

// ACT-AGM-09：项目登记完整地址、事件只给 owner/repo 时也要归到项目；读取器拿到的是登记的地址，不是简写。
func TestAgentsMDChangedEventMatchesRepoRegisteredByFullURL(t *testing.T) {
	const remote = "git@example.com:Acme/widgets.git"
	stdinLog := fakeReader(t, `{"commit":"abc123","branch":"main","path":"AGENTS.md","origin":"remote","content":"hand written\n"}`, 0)
	k := agentsKernelFor(t, remote)
	k.changedEvent(t, "e4")

	for _, spelling := range []string{agmRepo, remote} {
		v, err := k.agm.Get(context.Background(), spelling)
		if err != nil || v.LastCheck == nil || v.LastCheck.State != "drift" {
			t.Fatalf("%s: event must produce a drift check: %+v %v", spelling, v, err)
		}
	}
	if in, _ := os.ReadFile(stdinLog); !strings.Contains(string(in), `"repo":"`+remote+`"`) {
		t.Fatalf("reader must get the registered remote: %s", in)
	}
}

// 默认分支上的 AGENTS.md 被删（读取器退出码 3）是正常结论：按过期记检查，不报错。
func TestAgentsMDRemovedOnDefaultBranchIsStaleCheck(t *testing.T) {
	fakeReader(t, `{"error":{"code":"path_not_found","message":"gone"}}`, 3)
	k := agentsKernel(t)
	k.changedEvent(t, "e2")
	if v, err := k.agm.Get(context.Background(), agmRepo); err != nil || v.LastCheck == nil || v.LastCheck.State != "stale" {
		t.Fatalf("%+v %v", v, err)
	}
}

// 读取器失败要暴露真实错误（记日志），不伪造检查结论。
func TestAgentsMDReaderFailureIsLoggedAndRecordsNoCheck(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)
	fakeReader(t, `{"error":{"code":"unreadable","message":"remote down"}}`, 1)
	k := agentsKernel(t)
	k.changedEvent(t, "e3")
	if !strings.Contains(logs.String(), "remote down") {
		t.Fatalf("reader failure must be logged: %q", logs.String())
	}
	if v, _ := k.agm.Get(context.Background(), agmRepo); v.LastCheck != nil {
		t.Fatalf("no check may be recorded on failure: %+v", v.LastCheck)
	}
}
