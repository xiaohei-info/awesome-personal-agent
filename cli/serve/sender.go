package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/xiaohei-info/awesome-personal-agent/api/sources"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	kpacks "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	srcdecl "github.com/xiaohei-info/awesome-personal-agent/sources"
)

// webhookConfig 是 webhook 来源登记里与发件人有关的部分：用哪些映射，或转换脚本。
type webhookConfig struct {
	Mappings  []string        `json:"mappings"`
	Transform json.RawMessage `json:"transform"`
}

func webhookCfg(s domain.Source) (c webhookConfig) {
	_ = json.Unmarshal(s.Config, &c) // 登记时已校验；不是对象时当作两者都没有
	return
}

// senderCeiling 是事件落盘前的发件人上限（§9.1）：有接入声明按声明；没有声明的程序（自定义脚本、转换脚本）一律未核实；
// 只用映射的 webhook 来源没有程序，以映射的 sender_verified（验签结果）为准。
func senderCeiling(s domain.Source, eventType string) runner.SenderCeiling {
	if d, ok := srcdecl.Find(sourceDecls, string(s.Type), s.Command, s.SourceKind); ok {
		return runner.SenderCeiling(d.SenderFor(eventType))
	}
	if s.Type == domain.SourceWebhook && len(webhookCfg(s).Transform) == 0 {
		return runner.CeilingVerified
	}
	return runner.CeilingUnverified
}

// senderInfo 是对本人显示的来源结论（GET /sources 的 sender）：只有可信与需确认两种。
func senderInfo(ld *kpacks.Loader) func(domain.Source) sources.Sender {
	return func(s domain.Source) sources.Sender {
		if d, ok := srcdecl.Find(sourceDecls, string(s.Type), s.Command, s.SourceKind); ok {
			out := sources.Sender{Trust: d.Sender.Trust, Self: d.Sender.Self}
			if d.Sender.Hint.Zh != "" {
				out.Hint = &sources.Text{Zh: d.Sender.Hint.Zh, En: d.Sender.Hint.En}
			}
			return out
		}
		if s.Type == domain.SourceWebhook && signedMapping(ld, webhookCfg(s)) {
			return sources.Sender{Trust: srcdecl.TrustVerified}
		}
		return sources.Sender{Trust: srcdecl.TrustUnverified}
	}
}

// signedMapping：没有转换脚本，且所用映射里有按验签核实发件人的。
func signedMapping(ld *kpacks.Loader, c webhookConfig) bool {
	if len(c.Transform) != 0 {
		return false
	}
	for _, p := range ld.List() {
		for _, m := range p.WebhookMaps {
			for _, id := range c.Mappings {
				if m.ID == id && m.SenderVerified == "on_signature" {
					return true
				}
			}
		}
	}
	return false
}

// identityFetcher 经外呼调用器运行来源程序的 identity 与 fetch 模式（§9.2），同步等结果；结果不落事件。
type identityFetcher struct {
	st   *store.Store
	disp *proc.Dispatcher
	rn   *runner.Runner

	mu      sync.Mutex
	waiting map[string]chan proc.Result // Run id → 等待者
}

// deliver 截下 identity 运行的结果；不是它的返回 false。
func (f *identityFetcher) deliver(res proc.Result) bool {
	if res.Run.Category != domain.RunSource || !strings.HasPrefix(res.Run.Target, runner.IdentityPrefix) && !strings.HasPrefix(res.Run.Target, runner.FetchPrefix) {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.waiting[res.Run.ID]; ok {
		ch <- res // 缓冲 1，每个 Run 只交一次
		delete(f.waiting, res.Run.ID)
	}
	return true
}

func (f *identityFetcher) Fetch(ctx context.Context, s domain.Source) (string, error) {
	req, err := f.rn.IdentityPlan(s, clock.Now())
	if err != nil {
		return "", err
	}
	res, err := f.run(ctx, req)
	if err != nil {
		return "", err
	}
	return identityOf(res)
}

// run 入队一次运行并同步等它的终态结果。
func (f *identityFetcher) run(ctx context.Context, req proc.Request) (proc.Result, error) {
	ch := make(chan proc.Result, 1)
	f.mu.Lock()
	if f.waiting == nil {
		f.waiting = map[string]chan proc.Result{}
	}
	f.waiting[req.Run.ID] = ch
	f.mu.Unlock()
	defer func() { f.mu.Lock(); delete(f.waiting, req.Run.ID); f.mu.Unlock() }()
	if err := f.st.Update(ctx, func(tx *store.Tx) error { _, err := proc.Enqueue(tx, req, clock.Now()); return err }); err != nil {
		return proc.Result{}, err
	}
	if err := f.disp.Dispatch(ctx); err != nil {
		return proc.Result{}, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return proc.Result{}, ctx.Err()
	}
}

// FetchAttachment 运行来源程序的 fetch 模式取回第 n 个附件（§7.6）。失败、超时与超限都是 fetch_failed，原因取自程序的 stderr 末行。
func (f *identityFetcher) FetchAttachment(ctx context.Context, s domain.Source, ev domain.SourceEvent, n int, att runner.AttachmentRef) ([]byte, error) {
	req, err := f.rn.FetchPlan(s, ev, n, att, clock.Now())
	if err != nil {
		return nil, err
	}
	res, err := f.run(ctx, req)
	if err != nil {
		return nil, err
	}
	switch {
	case res.Overflow:
		return nil, fetchFailed("the attachment is larger than the 32 MiB limit")
	case res.Run.State != domain.RunSucceeded:
		why := res.Reason
		if line := lastLine(res.Stderr); line != "" {
			why = line
		}
		return nil, fetchFailed("the source program could not fetch it: " + why)
	}
	return res.Stdout, nil
}

func fetchFailed(why string) error {
	return domain.NewError(domain.CodeFetchFailed, why, "check that the message still exists in the source, or open the original message instead")
}

// lastLine 是 stderr 的最后一行非空文本（程序以 stderr 的一句话说明失败原因，§9.2）。
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// identityOf 取 stdout 末行的 {"sender"}；运行失败或没有给出时报错，前端据此不预填。
func identityOf(res proc.Result) (string, error) {
	var out struct {
		Sender string `json:"sender"`
	}
	lines := bytes.Split(bytes.TrimSpace(res.Stdout), []byte("\n"))
	if res.Run.State == domain.RunSucceeded && json.Unmarshal(lines[len(lines)-1], &out) == nil && strings.TrimSpace(out.Sender) != "" {
		return strings.TrimSpace(out.Sender), nil
	}
	why := "the source program gave no identity"
	if res.Reason != "" {
		why += ": " + res.Reason
	}
	return "", domain.NewError(domain.CodeConflict, why, "check the source's credentials, or type your identity by hand")
}
