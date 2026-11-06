// Package crash 是强杀实验（设计 §20、§6.14 第 1 条）：子进程循环写入账本，父进程在随机时刻 SIGKILL，
// 重启后检查每个事项的 version 与 Event 数一致、投影与事件折叠一致、SQLite 完整。
// 子进程是本测试二进制自己（TestMain 按环境变量切换），不依赖外部程序。
package crash_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	childEnv  = "APA_CRASH_CHILD_HOME"
	readyLine = "ready"
)

var bg = context.Background()

func openStore(home string) (*store.Store, error) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module} {
		if err := reg.Register(m); err != nil {
			return nil, err
		}
	}
	return store.Open(home, reg)
}

func TestMain(m *testing.M) {
	if home := os.Getenv(childEnv); home != "" {
		fmt.Fprintln(os.Stderr, "crash child:", writeForever(home))
		os.Exit(3) // 正常路径是被 SIGKILL，走到这里说明写入出错
	}
	os.Exit(m.Run())
}

// writeForever 不停地建事项、改标题、换阶段，每条命令都是一次独立事务。
func writeForever(home string) error {
	st, err := openStore(home)
	if err != nil {
		return err
	}
	x := command.New(st, time.Now)
	if err := items.Register(x, items.NewKinds()); err != nil {
		return err
	}
	owner := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	run := func(cmd string, in map[string]any) (json.RawMessage, error) {
		b, _ := json.Marshal(in)
		r, err := x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner"})
		return r.Value, err
	}
	for i := 0; ; i++ {
		raw, err := run("item.create", map[string]any{"title": fmt.Sprintf("item %d", i), "kind": "task", "phase": "open", "body": "payload"})
		if err != nil {
			return err
		}
		var out struct{ ID string }
		_ = json.Unmarshal(raw, &out)
		if i == 0 {
			fmt.Println(readyLine) // 第一个事项已落账：父进程从这里起算杀进程的随机延时
		}
		for j, n := 0, 1+rand.Intn(4); j < n; j++ {
			if _, err := run("item.update", map[string]any{"id": out.ID, "title": fmt.Sprintf("item %d rev %d", i, j)}); err != nil {
				return err
			}
		}
		if i%3 == 0 {
			if _, err := run("item.transition", map[string]any{"id": out.ID, "to": "dropped", "reason": "crash loop"}); err != nil {
				return err
			}
		}
	}
}

type snapshot struct {
	items  int
	events int
}

// check 在父进程里重开账本，逐个事项核对 version 与 Event 数；有任何不一致都返回错误。
func check(home string) (snapshot, error) {
	st, err := openStore(home)
	if err != nil {
		return snapshot{}, fmt.Errorf("reopen after kill: %w", err)
	}
	defer st.Close()
	var s snapshot
	err = st.View(bg, func(tx *store.Tx) error {
		rows, err := tx.Query("PRAGMA integrity_check")
		if err != nil {
			return err
		}
		var verdict string
		for rows.Next() {
			if err := rows.Scan(&verdict); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if verdict != "ok" {
			return fmt.Errorf("sqlite integrity_check: %s", verdict)
		}
		if err := items.Verify(tx); err != nil {
			return fmt.Errorf("projection diverges from events: %w", err)
		}
		all, err := items.List(tx, items.Filter{})
		if err != nil {
			return err
		}
		for _, it := range all {
			v, err := command.Version(tx, it.ID)
			if err != nil {
				return err
			}
			es, err := eventlog.ReadObject(tx, it.ID)
			if err != nil {
				return err
			}
			n := 0
			for _, e := range es {
				if e.Event.Type != domain.EvCommandRejected {
					n++
				}
			}
			if int64(n) != v || it.Version != v {
				return fmt.Errorf("item %s: version %d (projection %d) but %d events", it.ID, v, it.Version, n)
			}
			s.events += n
		}
		s.items = len(all)
		// 没有事项投影的对象也不能有事件：事件落盘而投影丢失会被 Verify 抓到，这里再核对总数。
		head, err := eventlog.Read(tx, 0, 1<<30)
		if err != nil {
			return err
		}
		if len(head) != s.events {
			return fmt.Errorf("ledger has %d events, items account for %d", len(head), s.events)
		}
		return nil
	})
	return s, err
}

func TestKillDuringWriteLoopKeepsVersionAndEventsAligned(t *testing.T) {
	home := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	var prev snapshot
	for round := 1; round <= 8; round++ {
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), childEnv+"="+home)
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// 等子进程真正开始写：CI 上进程启动与开库的耗时不定，固定睡眠可能在它写第一条之前就杀了，实验就什么也没证明。
		if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != readyLine {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("round %d: child never started writing (see stderr above)", round)
		}
		time.Sleep(time.Duration(300+rng.Intn(500)) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("round %d: child exited on its own (see stderr above): %v", round, err)
		}
		_ = cmd.Wait()
		if cmd.ProcessState.ExitCode() == 3 {
			t.Fatalf("round %d: child failed before it was killed", round)
		}
		got, err := check(home)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if got.events <= prev.events || got.items <= prev.items {
			t.Fatalf("round %d made no progress (%+v -> %+v): the experiment proves nothing", round, prev, got)
		}
		t.Logf("round %d: %d items, %d events consistent after SIGKILL", round, got.items, got.events)
		prev = got
	}
}
