package serve

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	apihealth "github.com/xiaohei-info/awesome-personal-agent/api/health"
	apiingest "github.com/xiaohei-info/awesome-personal-agent/api/ingest"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apijudgments "github.com/xiaohei-info/awesome-personal-agent/api/judgments"
	apimaterials "github.com/xiaohei-info/awesome-personal-agent/api/materials"
	apipacks "github.com/xiaohei-info/awesome-personal-agent/api/packs"
	apiprojects "github.com/xiaohei-info/awesome-personal-agent/api/projects"
	apiruns "github.com/xiaohei-info/awesome-personal-agent/api/runs"
	"github.com/xiaohei-info/awesome-personal-agent/api/settings/credentials"
	apiexec "github.com/xiaohei-info/awesome-personal-agent/api/settings/exec"
	apigrants "github.com/xiaohei-info/awesome-personal-agent/api/settings/grants"
	apioutbound "github.com/xiaohei-info/awesome-personal-agent/api/settings/outbound"
	apipolicy "github.com/xiaohei-info/awesome-personal-agent/api/settings/policy"
	apipurge "github.com/xiaohei-info/awesome-personal-agent/api/settings/purge"
	apisources "github.com/xiaohei-info/awesome-personal-agent/api/sources"
	apiversion "github.com/xiaohei-info/awesome-personal-agent/api/version"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/background"
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
	khealth "github.com/xiaohei-info/awesome-personal-agent/kernel/health"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/spotcheck"
	kmaterials "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	kpacks "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/followup"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/guideline"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/stale"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/verify"
	kprojects "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	kpurge "github.com/xiaohei-info/awesome-personal-agent/kernel/purge"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/resume"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/retention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	kscreening "github.com/xiaohei-info/awesome-personal-agent/kernel/screening"
	sregistry "github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/summaries"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/wake"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/web"
)

const (
	clockInterval = time.Second
	// packReloadInterval 是能力包热加载的检查间隔（§7.7），由时钟循环驱动；内容没变时只读文件算 hash，不写账本。
	packReloadInterval = 5 * time.Second
	// maintainInterval 是后台补做的间隔：重试判断失败的待跟进和未处理的入站记录、清过期 payload、补派发。
	maintainInterval = time.Minute
	owner            = "owner"
)

// bundledPull 是随发行附带的拉取程序（设计 §9.1 定时拉取：GitHub、RSS/Atom、本地目录监视、通用 HTTP JSON），delegate 可以登记。
// 邮件、日历适配器按 §9.7 与自定义脚本同级，只在 Web 或主机配置文件登记（§14.12）；托管型读取器不是来源，都不在其中。
// 新增 sources/*/cmd/* 程序必须在这里登记，或在 serve_test.go:notBundled 里写明不登记的理由，否则测试失败。
var bundledPull = []string{"http-json", "dir-watch", "github-source", "rss-source"}

// hostedReader 是只读读取器的 argv；托管文档与 AGENTS.md 触发共用，按名字解析（PATH）。
var hostedReader = []string{"hosted-reader"}

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
	ret    *retention.Service
	in     *inbound.Service
	eng    *rules.Engine
	fu     *followup.Service
	stale  *stale.Service
	scr    *kscreening.Service
	att    *attention.Attention
	hn     *khealth.Notifier
	rs     *resume.Service
	sum    *summaries.Service
	packs  *kpacks.Loader
	agm    *agentsmd.Service
	agmWG  sync.WaitGroup // 事件触发的 AGENTS.md 检测；测试收尾时等它们结束
	wk     *wake.Service

	resumeMu  sync.Mutex
	resumeDue []string // 决策到期按默认值决定的事项：时钟事务提交后再续接

	drainDue  atomic.Bool   // 本轮时钟排了规则或跟进的待办
	drain     chan struct{} // 唤醒后台循环：待办要调模型，不能占着时钟循环
	sumDue    atomic.Bool   // 有命令提交：摘要服务要追赶事件
	draft     chan struct{} // 唤醒决策卡片起草
	summarize chan struct{} // 唤醒摘要起草
	failed    []domain.Run  // 上次进程遗留、已按失败落账的运行，由后台循环交给各模块
}

func logErr(what string) func(error) {
	return func(err error) { slog.Error(what, "err", err) }
}

// assemble 构造全部内核服务、登记命令并挂载路由与 Web。apiURL 经运行凭据交给任务型运行者。
func assemble(st *store.Store, l config.Layout, cfg config.Config, apiURL string) (*kernel, error) {
	x := command.New(st, nil)
	x.MaxChainDepth = cfg.ChainDepthThreshold // 后台运行与续接沿用同一阈值（下方传入）
	kinds := kitems.NewKinds()
	ld := kpacks.New(kpacks.Options{Builtin: rootpacks.FS, DataDir: l.Packs, Kinds: kinds,
		OnReject: func(r kpacks.Rejection) {
			slog.Warn("pack rejected", "pack", r.Pack, "reason", r.Reason)
			if err := st.Update(context.Background(), func(tx *store.Tx) error { return khealth.PackRejected(tx, r) }); err != nil {
				slog.Error("pack rejection alert", "err", err)
			}
		}})
	if _, err := ld.Load(); err != nil {
		return nil, err
	}
	judge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}})
	if err != nil {
		return nil, err
	}
	// 初筛失败不重试（§14.4），要单独一个 Attempts=1 的实例；决策卡片文案与投递阈值挡在投递前，同样不重试。
	screenJudge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}, Attempts: 1})
	if err != nil {
		return nil, err
	}
	scr, err := kscreening.New(kscreening.Options{Store: st, Judge: screenJudge})
	if err != nil {
		return nil, err
	}

	q := delivery.New(delivery.Options{UnclaimedAfter: cfg.DeliveryUnclaimedAfter, Backoff: cfg.DeliveryBackoff})
	att := attention.New(attention.Options{Queue: q, Drafter: screenJudge})
	sum, err := summaries.New(summaries.Options{Store: st, Exec: x, Gen: judge, Owner: owner})
	if err != nil {
		return nil, err
	}
	k := &kernel{st: st, x: x, layout: l, scr: scr, att: att, sum: sum, hn: khealth.NewNotifier(q), packs: ld,
		drain: make(chan struct{}, 1), draft: make(chan struct{}, 1), summarize: make(chan struct{}, 1)}

	// 跟进消息归属与入站流水线互相接线：拦截在判断之前，退回后立即唤醒入站流水线。
	fm, err := followmsg.New(followmsg.Options{Store: st, Exec: x, Owner: owner,
		Wake: func(ctx context.Context) error { return k.in.Process(ctx) }, OnError: logErr("follow-up message")})
	if err != nil {
		return nil, err
	}
	rejudge := inbound.DefaultRejudge
	rejudge.Window = cfg.UnjudgedRejudgeWindow
	if k.in, err = inbound.New(inbound.Options{Store: st, Exec: x, Packs: ld, Judge: judge, Owner: owner,
		Intercept: fm.Intercept, OnError: logErr("inbound"), Rejudge: rejudge}); err != nil {
		return nil, err
	}
	if k.rn, err = runner.New(runner.Options{Store: st, Exec: x, Layout: l, OnError: logErr("source runner")}); err != nil {
		return nil, err
	}
	a := auth.New(nil)
	if k.wh, err = webhook.New(webhook.Options{Store: st, Exec: x, Packs: ld, Auth: a, Secrets: webhook.KeyFiles{Dir: l.WebhookKeys, Auth: a},
		Retention: cfg.IngestRetention, ScriptRoot: l.Sources, OnError: logErr("webhook")}); err != nil {
		return nil, err
	}
	if k.ret, err = retention.New(retention.Options{Store: st, Ingest: k.wh, Layout: l, RunRetention: cfg.RunRetention, JudgmentRetention: cfg.JudgmentRetention, KeepJudgments: cfg.JudgmentRetention == 0}); err != nil {
		return nil, err
	}
	verifyAgent, err := verify.Handler(verify.Options{Store: st, Judge: judge})
	if err != nil {
		return nil, err
	}
	// call_agent 按模板类别分派：任务型与生成型由任务型服务处理，判断型交委派核验。任务型服务要等调度器建好，闭包里再取。
	var tk *task.Service
	var hs *hosted.Service           // 托管型读取器的运行结果回到它；调度器建好后再赋值
	var draftGuideline rules.Handler // 缺规范检测（§12.6）：guideline-draft 包的 call_agent 交给它；任务型服务建好后再赋值
	callAgent := func(ctx context.Context, c rules.Call) error {
		if pack, _, _ := strings.Cut(c.Rule, "/"); pack == "guideline-draft" {
			return draftGuideline(ctx, c)
		}
		return tk.CallAgent(task.AgentOptions{Judge: verifyAgent, Generate: judge})(ctx, c)
	}
	var bg *background.Service // start_background_run 的处理器；调度器建好后再赋值
	startBackground := func(ctx context.Context, c rules.Call) error { return bg.Handler()(ctx, c) }
	if k.eng, err = rules.New(rules.Options{Store: st, Exec: x, Packs: ld, Owner: owner,
		Handlers: map[string]rules.Handler{"call_agent": callAgent, background.Action: startBackground}, Reminder: att, OnError: logErr("rules")}); err != nil {
		return nil, err
	}
	// 间隔、模板与模式取自 follow-up 包的 follow_up 时钟规则，跟进服务每次使用时从加载器读。
	if k.fu, err = followup.New(followup.Options{Store: st, Exec: x, Judge: judge, Reminder: att, Packs: ld, Owner: owner,
		Pack: "follow-up"}); err != nil {
		return nil, err
	}
	// 久未推进检查：阈值、周期与模式取自 follow-up 包的 stale 规则；"今天"取材用同一个阈值。
	if k.stale, err = stale.New(stale.Options{Packs: ld, Pack: "follow-up", Reminder: att}); err != nil {
		return nil, err
	}

	// 调度器的结果要回到 runner、webhook、task，而它们又要调度器：先建调度器，闭包里再取。
	var agmFetch agentsFetcher
	k.result = func(res proc.Result) {
		if agmFetch.deliver(res) { // AGENTS.md 读取的结果交给等待的 Fetch，不属于其他模块
			return
		}
		ctx := context.Background() // 关停时也要把终态落账
		if err := errors.Join(k.rn.Handle(ctx, res), k.wh.Handle(ctx, res)); err != nil {
			slog.Error("run result", "run", res.Run.ID, "err", err)
		}
		tk.Settle(ctx, res)
		if !k.rs.Settle(ctx, res) { // 续接因会话失效已改用新会话重新拉起时，不再按失败收尾
			bg.Settle(ctx, res)
		}
		hs.OnResult(res)
	}
	// 运行凭据在启动时签发并注入（§7.7）：任务型与后台运行各自签发，其他类别没有凭据。
	env := func(r domain.Run) map[string]string {
		if r.Category == domain.RunBackground {
			return bg.Env(r)
		}
		return tk.Env(r)
	}
	k.disp = proc.New(st, cfg, proc.Options{Env: env, ContextDir: l.RunCredentials, OnResult: k.result})
	agmFetch.st, agmFetch.disp, agmFetch.reader = st, k.disp, hostedReader
	host, herr := os.Hostname()
	if herr != nil {
		host = "localhost"
	}
	// 读取器随发行附带，与拉取程序一样按名字解析（PATH）；它是只读的子进程，凭据由 git 自身配置持有。
	if hs, err = hosted.New(x, hosted.Options{Reader: hostedReader, Host: host, Owner: owner,
		Kick: func() {
			if err := k.disp.Dispatch(context.Background()); err != nil {
				slog.Error("hosted reader dispatch", "err", err)
			}
		},
		OnError: func(material string, err error) { slog.Error("hosted material", "material", material, "err", err) }}); err != nil {
		return nil, err
	}
	k.rn.Attach(k.disp)
	k.wh.Attach(k.disp)
	if tk, err = task.New(task.Options{Store: st, Exec: x, Packs: ld, Queue: k.disp, RunsDir: l.Runs, APIURL: apiURL,
		OnError: func(run string, err error) { slog.Error("task run", "run", run, "err", err) }}); err != nil {
		return nil, err
	}
	if draftGuideline, err = guideline.Handler(guideline.Options{Store: st, Drafter: tk, Host: host, Executor: cfg.GuidelineDraftExecutor}); err != nil {
		return nil, err
	}
	// 唤醒（§13.6）：后台运行落到声明了 launch: wake 的 harness 时产出唤醒投递；换取凭据时开租约；回执后收尾，与调度器结果同一条路。
	if k.wk, err = wake.New(wake.Options{Delivery: q}); err != nil {
		return nil, err
	}
	wk := k.wk
	wk.Attach(x, k.result, logErr("wake receipt"))
	if bg, err = background.New(background.Options{Store: st, Exec: x, Packs: ld, Queue: k.disp, RunsDir: l.Runs, APIURL: apiURL, Owner: owner,
		Wake: wk, MaxChainDepth: x.MaxChainDepth, OnError: func(run string, err error) { slog.Error("background run", "run", run, "err", err) }}); err != nil {
		return nil, err
	}
	if k.rs, err = resume.New(resume.Options{Store: st, Packs: ld, Queue: k.disp, RunsDir: l.Runs, Owner: owner,
		MaxChainDepth: x.MaxChainDepth, OnError: func(run string, err error) { slog.Error("resume run", "run", run, "err", err) }}); err != nil {
		return nil, err
	}
	k.rs.Attach(x) // 回答与续接 Run 入队同一事务（§13.6）；事件规则随后命中时看到在途运行而跳过

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
		spotcheck.Register(x),
		fm.Register(x),
		rules.Register(x),
		instances.Register(x, ld),
		blobs.Register(x, l.Blobs),
		kmaterials.RegisterWith(x, ld), // 能力包声明的 material_kinds 与默认写入策略，提交时从加载器读
		kpurge.Register(x, l.Blobs),
		hosted.RegisterWith(x, ld),
		k.fu.Register(),
	); err != nil {
		return nil, err
	}
	x.OnCommit(scr.OnCommit)
	att.Attach(x, st, logErr("attention"))
	x.OnCommit(func(command.Commit) { k.sumDue.Store(true) })
	k.hn.Attach(x, st, logErr("health delivery"))
	k.fu.Attach(logErr("follow-up"))

	agm, ok := ld.Get("agents-md")
	if !ok || agm.Templates["agents"] == "" {
		return nil, errors.New("serve: agents-md pack has no render template")
	}
	agmSvc, err := agentsmd.New(x, agentsmd.Options{Template: agm.Templates["agents"], Fetch: agmFetch.Fetch, OnError: logErr("agents-md render")})
	if err != nil {
		return nil, err
	}

	k.agm = agmSvc
	x.OnCommit(triggerAgentsMD(agmSvc, &k.agmWG)) // 来源报告默认分支 AGENTS.md 变化 → 同一检测（§12.5）

	creds, err := credentials.Mount(x, a)
	if err != nil {
		return nil, err
	}
	grants, err := apigrants.Mount(x, a, st)
	if err != nil {
		return nil, err
	}
	sources, err := apisources.Mount(st, x, a, k.wh, k.in)
	if err != nil {
		return nil, err
	}
	ledger, err := schemaLedger()
	if err != nil {
		return nil, err
	}
	if k.srv, err = core.New(core.Deps{Store: st, Auth: a, Web: web.Handler(), Guard: wk.Identity, Groups: []core.RouteGroup{
		apiitems.Routes(x, st, k.stale.After), apidecisions.Routes(x, ""), apiprojects.Routes(st, x), apipolicy.Routes(st), creds, grants,
		apicapture.Routes(x), apicontext.Routes(st), apidelegation.Routes(x), apideliveries.Routes(st, q, 0), apichannel.Routes(x, st, nil),
		apifollowmsg.Routes(x), apiingest.Routes(k.wh, 0), apipacks.Routes(st, x, ld), apiruns.Routes(st, wk), apievents.Routes(st, 0),
		apiexec.Routes(st, x), apioutbound.Routes(st, x), apipurge.Routes(x), sources, apiwhy.Routes(st), apijudgments.Routes(st, &spotcheck.Service{Store: st, Exec: x, Proposer: k.in, Clock: clock.Now}), apihealth.Routes(st, q, ld, cfg.DelegationStallAfter), apimaterials.Routes(st, x, hs), apiagentsmd.Routes(x, k.agm), apiversion.Routes(ledger)}}); err != nil {
		return nil, err
	}
	k.clk, err = clock.New(st, k.handleTimer, nil)
	return k, err
}

// handleTimer 把到期 Timer 分派给所属模块；规则与跟进只在事务里排待办，执行要调模型，由后台循环做。
func (k *kernel) handleTimer(tx *store.Tx, c clock.Command) error {
	if err := khealth.RecordClockLag(tx, c, clock.Now()); err != nil { // 迟到只报不修，照常触发
		return err
	}
	switch {
	case k.att.Owns(c):
		return k.att.HandleTimer(tx, c)
	case kdecisions.Owns(c):
		// 到期按默认值决定也是回答：同一个时钟事务里入队续接（未决定的由续接自己跳过），提交后由时钟循环拉起进程
		if err := kdecisions.HandleTimer(tx, c); err != nil {
			return err
		}
		if _, err := k.rs.ResumeIn(tx, c.Target); err != nil {
			return err
		}
		k.resumeMu.Lock()
		k.resumeDue = append(k.resumeDue, c.Target)
		k.resumeMu.Unlock()
		return nil
	case k.rn.Owns(c):
		return k.rn.HandleTimer(tx, c)
	case instances.Owns(c):
		k.drainDue.Store(true) // instance.fired 由规则执行消费
		return instances.HandleTimer(tx, c)
	case k.stale.Owns(c):
		return k.stale.HandleTimer(tx, c)
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
	if err := k.st.Update(ctx, k.hn.Sync); err != nil { // 装配期的能力包拒绝与规则回退
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
	if err := k.st.Update(ctx, k.stale.Sync); err != nil {
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
		var nextReload time.Time
		for {
			report("clock tick failed", k.rn.Tick(ctx, k.clk))
			k.resumeMu.Lock()
			due := k.resumeDue
			k.resumeDue = nil
			k.resumeMu.Unlock()
			for _, id := range due {
				k.resume(id)
			}
			if now := clock.Now(); !now.Before(nextReload) {
				nextReload = now.Add(packReloadInterval)
				k.reloadPacks(ctx, report)
			}
			nudge(k.drainDue.Swap(false), k.drain)
			nudge(k.att.DraftDue(), k.draft)
			nudge(k.sumDue.Swap(false), k.summarize)
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			case <-clock.Changed(): // 测试构建：偏移前进后不等下一个 tick
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
			report("inbound rejudge", k.in.Rejudge(ctx)) // 模型恢复后重判积压、到期的转 expired（§9.3）
			report("webhook", k.wh.Process(ctx))
			report("retention", k.ret.Sweep(ctx))
			report("wake receipts", k.wk.Settle(ctx, k.st, k.result)) // 补上重启前漏掉的回执，先于调度器的超时判定
			report("dispatch", k.disp.Dispatch(ctx))
			report("rules", k.eng.Drain(ctx))
			report("follow-up", k.fu.Drain(ctx))
			k.syncHealth(ctx, report)
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
				k.syncHealth(ctx, report)
			case <-tk.C:
				maintain()
			}
		}
	}()
	// decision-card 包的起草要调模型，各用一个循环：模型慢或不可用时不挡规则与跟进，摘要积压也不拖住决策投递。
	// 启动时与每个维护间隔各做一次，补上重启前未完成的起草。
	k.worker(ctx, &wg, k.draft, func() {
		report("decision drafts", k.att.Drafts(ctx, k.st))
		k.syncHealth(ctx, report)
	})
	k.worker(ctx, &wg, k.summarize, func() {
		report("summaries", k.sum.Drain(ctx))
		k.syncHealth(ctx, report)
	})
	return func() {
		wg.Wait()
		k.disp.Close()
		k.scr.Wait()
		k.wh.Wait()
	}
}

// reloadPacks 是能力包热加载（§7.7）：重新扫描内置与数据目录，加载器按内容 hash 只重装变化的包。
// 生效的包有变化时，把持有包内容的下游对齐到新版本：规则状态（内容变了的规则回到 observe 并产出健康告警，由 rules.Reconcile 负责）、agents-md 渲染模板；
// 其余模块每次使用时从加载器取包，不需要通知。新的拒绝已由 OnReject 记账，这里把它们追赶成健康类投递。
func (k *kernel) reloadPacks(ctx context.Context, report func(string, error)) {
	before := k.packSig()
	fresh, err := k.packs.Load()
	report("pack reload", err)
	if err != nil {
		return
	}
	if k.packSig() != before {
		report("rules reconcile", k.eng.Reconcile(ctx))
		report("stale check", k.st.Update(ctx, k.stale.Sync))
		if p, ok := k.packs.Get("agents-md"); ok {
			report("agents-md template", k.agm.SetTemplate(p.Templates["agents"]))
		}
	}
	if len(fresh) > 0 || k.packSig() != before {
		k.syncHealth(ctx, report)
	}
}

// packSig 是生效包的名字与内容 hash，用来判断热加载有没有换掉什么。
func (k *kernel) packSig() string {
	var b strings.Builder
	for _, p := range k.packs.List() {
		b.WriteString(p.Name + "@" + p.Hash + "\n")
	}
	return b.String()
}

// resume 在到期按默认值决定后收尾（§13.6）：续接已在时钟事务里入队，这里拉起进程；不是等待续接的事项由 Resume 跳过。
// 拒绝与链路超限写下的健康告警不经命令提交，这里追赶成健康类投递。
func (k *kernel) resume(item string) {
	ctx := context.Background()
	if _, err := k.rs.Resume(ctx, item); err != nil {
		slog.Error("resume", "item", item, "err", err)
	}
	if err := k.st.Update(ctx, k.hn.Sync); err != nil {
		slog.Error("health delivery", "err", err)
	}
}

// nudge 在 due 时非阻塞地唤醒一个循环；已有未处理的唤醒就合并。
func nudge(due bool, ch chan struct{}) {
	if !due {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// worker 起一个循环：启动时、被唤醒时、每个维护间隔各跑一次 f，直到 ctx 取消。
func (k *kernel) worker(ctx context.Context, wg *sync.WaitGroup, wakeUp <-chan struct{}, f func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		f()
		tk := time.NewTicker(maintainInterval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wakeUp:
				f()
			case <-tk.C:
				f()
			}
		}
	}()
}

// syncHealth 追赶不经命令提交写下的健康告警（规则动作失败、跟进失败等），命令提交的由 Attach 立即追赶。
func (k *kernel) syncHealth(ctx context.Context, report func(string, error)) {
	report("health delivery", k.st.Update(ctx, k.hn.Sync))
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
