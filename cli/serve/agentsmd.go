package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// agentsMDChanged 是 GitHub 拉取程序在默认分支 AGENTS.md 变化时产出的事件类型（设计 §12.5）。
const agentsMDChanged = "repo.agents_md_changed"

// agentsTarget 标记 Fetch 入队的 reader Run：结果交给等待者，不交托管文档（它的 Target 是材料 id）。
const agentsTarget = "agents-md:"

const agentsFetchTimeout = 30 // 秒，与托管文档读取同一默认值

// agentsFetcher 经外呼调用器拉起只读读取器，读仓库默认分支根目录的 AGENTS.md，同步等结果。
type agentsFetcher struct {
	st     *store.Store
	disp   *proc.Dispatcher
	reader []string

	one  sync.Mutex // 一次只读一个仓库：事件稀少，省掉按 Run 匹配的表
	mu   sync.Mutex
	want string
	got  chan proc.Result
}

// deliver 在 Run 终态交给等待者；是本读取的 Run 就返回 true，调用方不再把它交给其他模块。
func (f *agentsFetcher) deliver(res proc.Result) bool {
	if res.Run.Category != domain.RunReader || len(res.Run.Target) < len(agentsTarget) || res.Run.Target[:len(agentsTarget)] != agentsTarget {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.got != nil && res.Run.Target == f.want {
		f.got <- res // 缓冲 1，且 want 同时只有一个 Run
	}
	return true
}

func (f *agentsFetcher) Fetch(ctx context.Context, repo string) (string, bool, error) {
	f.one.Lock()
	defer f.one.Unlock()
	ch := make(chan proc.Result, 1)
	f.mu.Lock()
	f.want, f.got = agentsTarget+repo, ch
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.got = nil; f.mu.Unlock() }()

	stdin, _ := json.Marshal(map[string]any{"source_id": f.want, "config": map[string]string{"repo": repo, "path": agentsmd.FileName}})
	err := f.st.Update(ctx, func(tx *store.Tx) error {
		_, err := proc.Enqueue(tx, proc.Request{
			Run:  domain.Run{Category: domain.RunReader, Target: f.want, TimeoutSec: agentsFetchTimeout},
			Spec: proc.Spec{Command: f.reader, Stdin: stdin},
		}, time.Now())
		return err
	})
	if err != nil {
		return "", false, err
	}
	if err := f.disp.Dispatch(ctx); err != nil {
		return "", false, err
	}
	select {
	case res := <-ch:
		return readerContent(res)
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

// readerContent 解读读取器结果：退出码 3 是默认分支上没有该文件（正常结论），其余失败原样报错。
func readerContent(res proc.Result) (string, bool, error) {
	var out struct {
		Commit  string `json:"commit"`
		Content string `json:"content"`
		Error   *struct{ Message string }
	}
	lines := bytes.Split(bytes.TrimSpace(res.Stdout), []byte("\n"))
	parsed := json.Unmarshal(lines[len(lines)-1], &out) == nil
	switch {
	case res.Run.State == domain.RunSucceeded && parsed && out.Commit != "":
		return out.Content, true, nil
	case res.Run.ExitCode != nil && *res.Run.ExitCode == 3:
		return "", false, nil
	case parsed && out.Error != nil:
		return "", false, fmt.Errorf("reader: %s", out.Error.Message)
	}
	return "", false, fmt.Errorf("reader %s: %s", res.Run.State, res.Reason)
}

// triggerAgentsMD 把来源落账的 repo.agents_md_changed 事件路由到 agentsmd.Trigger（§12.5）。
// 读取要等外呼进程，所以放到后台做，不占提交后的回调；失败只能记录，下次文件再变化会重新触发。
func triggerAgentsMD(svc *agentsmd.Service, wg *sync.WaitGroup) func(command.Commit) {
	return func(c command.Commit) {
		if c.Command != runner.CmdRunResult && c.Command != runner.CmdIngest {
			return
		}
		var in struct {
			Events []struct {
				Type            string   `json:"type"`
				CorrelationKeys []string `json:"correlation_keys"`
			} `json:"events"`
		}
		if json.Unmarshal(c.Input, &in) != nil {
			return
		}
		for _, ev := range in.Events {
			if ev.Type != agentsMDChanged || len(ev.CorrelationKeys) == 0 {
				continue
			}
			repo := ev.CorrelationKeys[0]
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := svc.Trigger(context.Background(), repo, []string{agentsmd.FileName}); err != nil {
					slog.Error("agents-md trigger", "repo", repo, "err", err)
				}
			}()
		}
	}
}
