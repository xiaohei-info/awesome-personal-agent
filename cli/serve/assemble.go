package serve

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apiagentsmd "github.com/xiaohei-info/awesome-personal-agent/api/agentsmd"
	apicapture "github.com/xiaohei-info/awesome-personal-agent/api/capture"
	apichannel "github.com/xiaohei-info/awesome-personal-agent/api/channel"
	apicontext "github.com/xiaohei-info/awesome-personal-agent/api/context"
	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apidecisions "github.com/xiaohei-info/awesome-personal-agent/api/decisions"
	apidelegation "github.com/xiaohei-info/awesome-personal-agent/api/delegation"
	apideliveries "github.com/xiaohei-info/awesome-personal-agent/api/deliveries"
	apievents "github.com/xiaohei-info/awesome-personal-agent/api/events"
	apifollowmsg "github.com/xiaohei-info/awesome-personal-agent/api/followmsg"
	apiingest "github.com/xiaohei-info/awesome-personal-agent/api/ingest"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apipacks "github.com/xiaohei-info/awesome-personal-agent/api/packs"
	apiprojects "github.com/xiaohei-info/awesome-personal-agent/api/projects"
	apiruns "github.com/xiaohei-info/awesome-personal-agent/api/runs"
	"github.com/xiaohei-info/awesome-personal-agent/api/settings/credentials"
	apiexec "github.com/xiaohei-info/awesome-personal-agent/api/settings/exec"
	apipolicy "github.com/xiaohei-info/awesome-personal-agent/api/settings/policy"
	apisources "github.com/xiaohei-info/awesome-personal-agent/api/sources"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	kchannel "github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	kdecisions "github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	kpacks "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/followup"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/verify"
	kprojects "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	kscreening "github.com/xiaohei-info/awesome-personal-agent/kernel/screening"
	sregistry "github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/web"
)

const (
	clockInterval = time.Second
	// maintainInterval 是后台补做的间隔：重试判断失败的待跟进和未处理的入站记录、清过期 payload、补派发。
	maintainInterval = time.Minute
	owner            = "owner"
)

// bundledPull 是随发行附带的拉取程序（设计 §9.1 定时拉取：GitHub、RSS/Atom、本地目录监视、通用 HTTP JSON），delegate 可以登记。
// 邮件、日历适配器按 §9.7 与自定义脚本同级，只在 Web 或主机配置文件登记（§14.12）；托管型读取器不是来源，都不在其中。
// 新增 sources/*/cmd/* 程序必须在这里登记，或在 serve_test.go:notBundled 里写明不登记的理由，否则测试失败。
var bundledPull = []string{"http-json", "dir-watch", "github-source", "rss-source"}

// hostOwner 是启动流程读主机配置文件时的身份：本人在主机上写的配置，与 Web 上的本人同级（§14.12）。
var hostOwner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: owner, Via: domain.ViaWeb}, Level: authz.LevelWeb}

// kernel 是装配好的内核：assemble 只构造和登记，不写账本、不起进程，这些都在 start 与 runLoops。
type kernel struct {
	st     *store.Store
	x      *command.Executor
	layout config.Layout
	srv    *core.Server
	clk    *clock.Clock
	disp   *proc.Dispatcher
	result func(proc.Result) // 运行终态的分派，与调度器的 OnResult 同一条路
	rn     *runner.Runner
	wh     *webhook.Service
	in     *inbound.Service
	eng    *rules.Engine
	fu     *followup.Service
	scr    *kscreening.Service
	att    *attention.Attention

	drainDue atomic.Bool   // 本轮时钟排了规则或跟进的待办
	drain    chan struct{} // 唤醒后台循环：待办要调模型，不能占着时钟循环
	failed   []domain.Run  // 上次进程遗留、已按失败落账的运行，由后台循环交给各模块
}

func logErr(what string) func(error) {
	return func(err error) { slog.Error(what, "err", err) }
}

// assemble 构造全部内核服务、登记命令并挂载路由与 Web。apiURL 经运行凭据交给任务型运行者。
func assemble(st *store.Store, l config.Layout, cfg config.Config, apiURL string) (*kernel, error) {
	x := command.New(st, nil)
	kinds := kitems.NewKinds()
	ld := kpacks.New(kpacks.Options{Builtin: rootpacks.FS, DataDir: l.Packs, Kinds: kinds,
		OnReject: func(r kpacks.Rejection) { slog.Warn("pack rejected", "pack", r.Pack, "reason", r.Reason) }})
	if _, err := ld.Load(); err != nil {
		return nil, err
	}
	judge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}})
	if err != nil {
		return nil, err
	}
	// 初筛失败不重试（§14.4），要单独一个 Attempts=1 的实例。
	screenJudge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}, Attempts: 1})
	if err != nil {
		return nil, err
	}
	scr, err := kscreening.New(kscreening.Options{Store: st, Judge: screenJudge})
	if err != nil {
		return nil, err
	}

	q := delivery.New(delivery.Options{})
	att := attention.New(attention.Options{Queue: q})
	k := &kernel{st: st, x: x, layout: l, scr: scr, att: att, drain: make(chan struct{}, 1)}

	// 跟进消息归属与入站流水线互相接线：拦截在判断之前，退回后立即唤醒入站流水线。
	fm, err := followmsg.New(followmsg.Options{Store: st, Exec: x, Owner: owner,
		Wake: func(ctx context.Context) error { return k.in.Process(ctx) }, OnError: logErr("follow-up message")})
	if err != nil {
		return nil, err
	}
	if k.in, err = inbound.New(inbound.Options{Store: st, Exec: x, Packs: ld, Judge: judge, Owner: owner,
		Intercept: fm.Intercept, OnError: logErr("inbound")}); err != nil {
		return nil, err
	}
	if k.rn, err = runner.New(runner.Options{Store: st, Exec: x, Delivery: q, Layout: l, OnError: logErr("source runner")}); err != nil {
		return nil, err
	}
	a := auth.New(nil)
	if k.wh, err = webhook.New(webhook.Options{Store: st, Exec: x, Packs: ld, Auth: a, Secrets: webhook.KeyFiles{Dir: l.WebhookKeys, Auth: a},
		Retention: cfg.IngestRetention, ScriptRoot: l.Sources, OnError: logErr("webhook")}); err != nil {
		return nil, err
	}
	verifyAgent, err := verify.Handler(verify.Options{Store: st, Judge: judge})
	if err != nil {
		return nil, err
	}
	// call_agent 按模板类别分派：任务型与生成型由任务型服务处理，判断型交委派核验。任务型服务要等调度器建好，闭包里再取。
	var tk *task.Service
	callAgent := func(ctx context.Context, c rules.Call) error {
		return tk.CallAgent(task.AgentOptions{Judge: verifyAgent, Generate: judge})(ctx, c)
	}
	if k.eng, err = rules.New(rules.Options{Store: st, Exec: x, Packs: ld, Owner: owner,
		Handlers: map[string]rules.Handler{"call_agent": callAgent}, Reminder: att, OnError: logErr("rules")}); err != nil {
		return nil, err
	}
	if k.fu, err = followup.New(followup.Options{Store: st, Exec: x, Judge: judge, Reminder: att, Owner: owner,
		Pack: "follow-up", Template: "follow-up-judge"}); err != nil {
		return nil, err
	}

	// 调度器的结果要回到 runner、webhook、task，而它们又要调度器：先建调度器，闭包里再取。
	k.result = func(res proc.Result) {
		ctx := context.Background() // 关停时也要把终态落账
		if err := errors.Join(k.rn.Handle(ctx, res), k.wh.Handle(ctx, res)); err != nil {
			slog.Error("run result", "run", res.Run.ID, "err", err)
		}
		tk.Settle(ctx, res)
	}
	k.disp = proc.New(st, cfg, proc.Options{Env: func(r domain.Run) map[string]string { return tk.Env(r) }, OnResult: k.result})
	k.rn.Attach(k.disp)
	k.wh.Attach(k.disp)
	if tk, err = task.New(task.Options{Store: st, Exec: x, Packs: ld, Queue: k.disp, RunsDir: l.Runs, APIURL: apiURL,
		OnError: func(run string, err error) { slog.Error("task run", "run", run, "err", err) }}); err != nil {
		return nil, err
	}

	if err := errors.Join(
		kitems.Register(x, kinds),
		kdecisions.RegisterWith(x, scr),
		kchannel.Register(x, kchannel.Options{Queue: q}),
		kprojects.Register(x),
		executors.Register(x, executors.Options{Owner: owner, Project: kprojects.Get}),
		delegation.Register(x, delegation.Options{Owner: owner, OnError: func(c command.Commit, err error) {
			slog.Error("delegation settle", "object", c.Object, "err", err)
		}}),
		sregistry.Register(x, sregistry.Options{Bundled: bundledPull, Layout: l}),
		k.rn.Register(x),
		k.in.Register(x),
		fm.Register(x),
		rules.Register(x),
		instances.Register(x, ld),
		blobs.Register(x, l.Blobs),
		k.fu.Register(),
	); err != nil {
		return nil, err
	}
	x.OnCommit(scr.OnCommit)
	att.Attach(x, st, logErr("attention"))
	k.fu.Attach(logErr("follow-up"))

	agm, ok := ld.Get("agents-md")
	if !ok || agm.Templates["agents"] == "" {
		return nil, errors.New("serve: agents-md pack has no render template")
	}
	agmSvc, err := agentsmd.New(x, agentsmd.Options{Template: agm.Templates["agents"], OnError: logErr("agents-md render")})
	if err != nil {
		return nil, err
	}

	creds, err := credentials.Mount(x, a)
	if err != nil {
		return nil, err
	}
	sources, err := apisources.Mount(st, x, a, k.wh, k.in)
	if err != nil {
		return nil, err
	}
	if k.srv, err = core.New(core.Deps{Store: st, Auth: a, Web: web.Handler(), Groups: []core.RouteGroup{
		apiitems.Routes(x, st), apidecisions.Routes(x, ""), apiprojects.Routes(st, x), apipolicy.Routes(st), creds,
		apicapture.Routes(x), apicontext.Routes(st), apidelegation.Routes(x), apideliveries.Routes(st, q, 0), apichannel.Routes(x, st, nil),
		apifollowmsg.Routes(x), apiingest.Routes(k.wh, 0), apipacks.Routes(st, x, ld), apiruns.Routes(st), apievents.Routes(st, 0),
		apiexec.Routes(st, x), sources, apiwhy.Routes(st), apiagentsmd.Routes(x, agmSvc)}}); err != nil {
		return nil, err
	}
	k.clk, err = clock.New(st, k.handleTimer, nil)
	return k, err
}

// handleTimer 把到期 Timer 分派给所属模块；规则与跟进只在事务里排待办，执行要调模型，由后台循环做。
func (k *kernel) handleTimer(tx *store.Tx, c clock.Command) error {
	switch {
	case k.att.Owns(c):
		return k.att.HandleTimer(tx, c)
	case kdecisions.Owns(c):
		return kdecisions.HandleTimer(tx, c)
	case k.rn.Owns(c):
		return k.rn.HandleTimer(tx, c)
	case instances.Owns(c):
		k.drainDue.Store(true) // instance.fired 由规则执行消费
		return instances.HandleTimer(tx, c)
	case k.fu.Owns(c):
		k.drainDue.Store(true)
		return k.fu.HandleTimer(tx, c)
	case rules.Owns(c):
		k.drainDue.Store(true)
		return k.eng.HandleTimer(tx, c)
	}
	return kitems.HandleTimer(tx, c)
}

// start 按序做启动时的一次性工作：对齐规则与能力包 → 读主机配置 → 对齐时间轮 → 恢复上次进程遗留的状态。
// 这里只做快操作；可能调模型的补处理放在后台循环，不挡服务启动。
func (k *kernel) start(ctx context.Context) error {
	// 无法落成 Timer 的 schedule 是某个包的问题，规则状态已登记，不挡启动。
	if err := k.eng.Reconcile(ctx); err != nil {
		slog.Error("rules reconcile", "err", err)
	}
	if err := k.loadHostFiles(ctx); err != nil {
		return err
	}
	if err := k.rn.Sync(ctx); err != nil {
		return err
	}
	if err := k.st.Update(ctx, k.att.Start); err != nil {
		return err
	}
	if err := k.st.Update(ctx, k.fu.Sync); err != nil {
		return err
	}
	failed, alive, err := k.disp.Recover(ctx)
	if err != nil {
		return err
	}
	k.failed = failed
	for _, r := range alive {
		// shortcut: 存活的进程组没有接管逻辑，继续占额度直到下次重启判定；后台运行续接落地时接管。
		slog.Warn("run still alive after restart", "run", r.ID)
	}
	_, err = k.scr.Recover(ctx)
	return err
}

// loadHostFiles 读本人在主机上写的登记（§14.12）：文件不存在就跳过；内容有误拒绝启动，不带着半份配置运行。
func (k *kernel) loadHostFiles(ctx context.Context) error {
	if err := sregistry.LoadFile(ctx, k.x, hostOwner, k.layout.HostSources); err != nil && !errors.Is(err, os.ErrNotExist) {
		return usage(err.Error(), "fix "+k.layout.HostSources)
	}
	files, err := filepath.Glob(filepath.Join(k.layout.HostDeclarations, "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		if _, err := executors.RegisterHostFile(ctx, k.x, hostOwner, f); err != nil {
			return usage(err.Error(), "fix "+f)
		}
	}
	return nil
}

// runLoops 启动时钟与后台循环，返回的函数在 ctx 取消后调用：等循环退出、关停子进程（终态落账）、等异步处理结束。
func (k *kernel) runLoops(ctx context.Context) (stopped func()) {
	report := func(what string, err error) {
		if err != nil && ctx.Err() == nil {
			slog.Error(what, "err", err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(clockInterval)
		defer tk.Stop()
		for {
			report("clock tick failed", k.rn.Tick(ctx, k.clk))
			if k.drainDue.Swap(false) {
				select {
				case k.drain <- struct{}{}:
				default:
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
		}
	}()
	go func() {
		defer wg.Done()
		for _, r := range k.failed {
			k.result(proc.Result{Run: r, Reason: "no live process after restart"})
		}
		report("webhook recover", k.wh.Recover(ctx))
		maintain := func() {
			report("inbound", k.in.Process(ctx))
			report("webhook", k.wh.Process(ctx))
			report("webhook expire", k.wh.Expire(ctx))
			report("dispatch", k.disp.Dispatch(ctx))
			report("rules", k.eng.Drain(ctx))
			report("follow-up", k.fu.Drain(ctx))
		}
		maintain()
		tk := time.NewTicker(maintainInterval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-k.drain:
				report("rules", k.eng.Drain(ctx))
				report("follow-up", k.fu.Drain(ctx))
			case <-tk.C:
				maintain()
			}
		}
	}()
	return func() {
		wg.Wait()
		k.disp.Close()
		k.scr.Wait()
		k.wh.Wait()
	}
}

// localURL 是本机进程访问内核的地址；监听通配地址时走回环。
func localURL(a net.Addr) string {
	t := a.(*net.TCPAddr)
	host := t.IP.String()
	if t.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(t.Port))
}
