package serve

import (
	"context"
	"encoding/json"
	"errors"
	srcdecl "github.com/xiaohei-info/awesome-personal-agent/sources"
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
	apiconnections "github.com/xiaohei-info/awesome-personal-agent/api/connections"
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
	apioutbound "github.com/xiaohei-info/awesome-personal-agent/api/settings/outbound"
	apipolicy "github.com/xiaohei-info/awesome-personal-agent/api/settings/policy"
	apipurge "github.com/xiaohei-info/awesome-personal-agent/api/settings/purge"
	apiretention "github.com/xiaohei-info/awesome-personal-agent/api/settings/retention"
	apisources "github.com/xiaohei-info/awesome-personal-agent/api/sources"
	apiversion "github.com/xiaohei-info/awesome-personal-agent/api/version"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"
	"github.com/xiaohei-info/awesome-personal-agent/connectors/bark"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct/rulejudge"
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
	kconnections "github.com/xiaohei-info/awesome-personal-agent/kernel/connections"
	kdecisions "github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/metrics"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	kpacks "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/followup"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/guideline"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/stale"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/verify"
	kprojects "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	kpurge "github.com/xiaohei-info/awesome-personal-agent/kernel/purge"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/resume"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/retention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
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
	// 每天清理运行数据的时刻（本地时间）：避开整点与半点，也避开已知的重启窗口。
	sweepHour, sweepMinute = 4, 10
)

// bundledPull 是随发行附带的拉取程序（设计 §9.1 定时拉取：GitHub、RSS/Atom、本地目录监视、通用 HTTP JSON），delegate 可以登记。
// 邮件、日历适配器按 §9.7 与自定义脚本同级，只在 Web 或主机配置文件登记（§14.12）；托管型读取器不是来源，都不在其中。
// 新增 sources/*/cmd/* 程序必须在这里登记，或在 serve_test.go:notBundled 里写明不登记的理由，否则测试失败。
var bundledPull = []string{"http-json", "dir-watch", "github-source", "rss-source"}

// sourceDecls 是仓库自带来源程序的接入声明（sources/*/source.yaml，嵌入主程序，§9.1）；不合法是构建缺陷，由 sources 的测试守住。
var sourceDecls = func() []srcdecl.Declaration {
	ds, err := srcdecl.Load()
	if err != nil {
		panic(err)
	}
	return ds
}()

// declaredName 是接入层为登记 s 声明的名字（lang 为 zh 或 en）；没有声明返回空串。
func declaredName(s domain.Source, lang string) string {
	d, ok := srcdecl.Find(sourceDecls, string(s.Type), s.Command, s.SourceKind)
	if !ok {
		return ""
	}
	return d.NameFor(s.Config, lang)
}

// storedName 是登记没给名字时存下的声明名：账本里只存一种语言的名字（本人改名后也只有一种），
// 取中文，与通知、简报、卡片的语言一致；Web 新登记表单按界面语言预填，提交的就是本人看到的那一个。
func storedName(s domain.Source) string { return declaredName(s, "zh") }

// sourceKindNames 只用于升级补名：没有接入层声明的旧来源按 source_type 起的兜底名（§16.5）；产品名不进内核，在这里给出。
// 没列的 source_type 由内核按来源类型起名。
var sourceKindNames = map[string]string{"mail": "邮件", "calendar": "日历", "reminder": "提醒事项", "github": "GitHub"}

// builtinConnectors 是随主程序编译的连接插件（connectors/<类型>/，§3）；内核不 import 它们，由这里交给外呼调用器。
var builtinConnectors = func() []conn.Plugin { return []conn.Plugin{bark.Plugin()} }

// senderInterval 是渠道发送器领取投递的间隔。
const senderInterval = 2 * time.Second

// hostedReader 是只读读取器的 argv；托管文档与 AGENTS.md 触发共用，按名字解析（PATH）。
var hostedReader = []string{"hosted-reader"}

// hostOwner 是启动流程读主机配置文件时的身份：本人在主机上写的配置，与 Web 上的本人同级（§14.12）。
// 主机终端是恢复通道，按"刚验过口令"处理，3b 的登记照旧生效。
// shortcut: 方案要求主机文件里新增或改动的可执行登记进"等你确认"；还没做，文件改动仍在启动时直接生效。
var hostOwner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: owner, Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}

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
	att    *attention.Attention
	push   *khealth.Pusher
	hn     *khealth.Notifier
	rs     *resume.Service
	sum    *summaries.Service
	packs  *kpacks.Loader
	agm    *agentsmd.Service
	agmWG  sync.WaitGroup  // 事件触发的 AGENTS.md 检测；测试收尾时等它们结束
	ids    identityFetcher // 来源 identity 模式（§9.2）
	wk     *wake.Service
	send   *kchannel.Sender // 渠道发送器：内置渠道连接领取投递并经外呼调用器发送（§2.2）

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
	// 决策卡片文案与投递阈值挡在投递前，失败不重试，要单独一个 Attempts=1 的实例。
	screenJudge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}, Attempts: 1})
	if err != nil {
		return nil, err
	}

	// 连接（§1.5）：凭据保管库的主密钥缺失时生成新的，照常启动，旧凭据显示要重新填写（§5.5）。
	vault, created, err := secrets.Open(context.Background(), st, l.Home)
	if err != nil {
		return nil, err
	}
	if created {
		slog.Info("generated a new master key for connection credentials", "file", secrets.KeyFile)
	}
	caller, err := conn.New(conn.Options{Store: st, Vault: vault, Plugins: builtinConnectors()})
	if err != nil {
		return nil, err
	}
	conns := kconnections.New(vault, caller.Decl)
	q := delivery.New(delivery.Options{UnclaimedAfter: cfg.DeliveryUnclaimedAfter, Backoff: cfg.DeliveryBackoff, Deliverable: kconnections.Deliverable})
	sum, err := summaries.New(summaries.Options{Store: st, Exec: x, Gen: judge, Owner: owner})
	if err != nil {
		return nil, err
	}
	base := cfg.PublicURL
	if base == "" {
		base = apiURL
	}
	// 投递里的事项链接用绝对地址：注入主会话的消息只有文字，相对路径点不开。
	att := attention.New(attention.Options{Queue: q, Drafter: screenJudge, WebURL: func(id string) string { return strings.TrimRight(base, "/") + "/items/" + id }})
	k := &kernel{st: st, x: x, layout: l, att: att, sum: sum, hn: khealth.NewNotifier(att), push: khealth.NewPusher(att), packs: ld,
		send:  &kchannel.Sender{Store: st, Queue: q, Conns: conns, Call: caller, Base: base},
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
		Intercept: fm.Intercept, Attaches: fm.Attaches, OnError: logErr("inbound"), Rejudge: rejudge, BacklogReleasePerDay: cfg.BacklogReleasePerDay,
		Series: metrics.Series{}, Alert: &metrics.Alerter{Attention: att}}); err != nil {
		return nil, err
	}
	if k.rn, err = runner.New(runner.Options{Store: st, Exec: x, Layout: l, MaxEvent: cfg.MaxSourceEvent, OnError: logErr("source runner"), SenderCeiling: senderCeiling, Owner: owner}); err != nil {
		return nil, err
	}
	a := auth.New(nil)
	if k.wh, err = webhook.New(webhook.Options{Store: st, Exec: x, Packs: ld, Auth: a, Secrets: webhook.KeyFiles{Dir: l.WebhookKeys, Auth: a},
		MaxBody: cfg.MaxWebhookBody, MaxEvent: cfg.MaxSourceEvent, Retention: cfg.IngestRetention, ScriptRoot: l.Sources, OnError: logErr("webhook")}); err != nil {
		return nil, err
	}
	if k.ret, err = retention.New(retention.Options{Store: st, Ingest: k.wh, Layout: l, Exec: x, RunRetention: cfg.RunRetention, IngestRetention: cfg.IngestRetention, JudgmentRetention: cfg.JudgmentRetention, KeepJudgments: cfg.JudgmentRetention == 0}); err != nil {
		return nil, err
	}
	verifyAgent, err := verify.Handler(verify.Options{Store: st, Judge: judge})
	if err != nil {
		return nil, err
	}
	// 判断型：委派回执核验包（要求触发事项是委派子事项）走核验处理器，其余包的判断型模板只写判断记录。
	genericJudge := rulejudge.Handler(judge)
	judgeAgent := func(ctx context.Context, c rules.Call) error {
		if pack, _, _ := strings.Cut(c.Rule, "/"); pack == "delegation-verify" {
			return verifyAgent(ctx, c)
		}
		return genericJudge(ctx, c)
	}
	// call_agent 按模板类别分派：任务型与生成型由任务型服务处理，判断型见上。任务型服务要等调度器建好，闭包里再取。
	var tk *task.Service
	var hs *hosted.Service           // 托管型读取器的运行结果回到它；调度器建好后再赋值
	var draftGuideline rules.Handler // 缺规范检测（§12.6）：guideline-draft 包的 call_agent 交给它；任务型服务建好后再赋值
	callAgent := func(ctx context.Context, c rules.Call) error {
		if pack, _, _ := strings.Cut(c.Rule, "/"); pack == "guideline-draft" {
			return draftGuideline(ctx, c)
		}
		return tk.CallAgent(task.AgentOptions{Judge: judgeAgent, Generate: judge})(ctx, c)
	}
	var bg *background.Service // start_background_run 的处理器；调度器建好后再赋值
	startBackground := func(ctx context.Context, c rules.Call) error { return bg.Handler()(ctx, c) }
	if k.eng, err = rules.New(rules.Options{Store: st, Exec: x, Packs: ld, Owner: owner,
		Handlers: map[string]rules.Handler{"call_agent": callAgent, background.Action: startBackground}, Reminder: att, OnError: logErr("rules"),
		Ready: map[string]rules.Ready{background.Action: func(ctx context.Context, c rules.Call) (string, error) { return bg.Ready(ctx, c) }},
		Gate: func(ctx context.Context, pack string, do []kpacks.Action, params json.RawMessage) (string, error) {
			return tk.Gate(ctx, pack, do, params)
		}}); err != nil {
		return nil, err
	}
	// 间隔、轮数与模式取自 follow-up 包的 follow_up 时钟规则，跟进服务每次使用时从加载器读。
	if k.fu, err = followup.New(followup.Options{Store: st, Exec: x, Reminder: att, Packs: ld, Owner: owner,
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
		if agmFetch.deliver(res) || k.ids.deliver(res) { // AGENTS.md 读取与 identity 模式的结果交给等待者，不属于其他模块
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
	k.disp = proc.New(st, cfg, proc.Options{CheckAgent: executors.CheckRun, Env: env, ContextDir: l.RunCredentials, OnResult: k.result})
	agmFetch.st, agmFetch.disp, agmFetch.reader = st, k.disp, hostedReader
	k.ids.st, k.ids.disp, k.ids.rn = st, k.disp, k.rn
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
	if tk, err = task.New(task.Options{Store: st, Exec: x, Packs: ld, Queue: k.disp, RunsDir: l.Runs, APIURL: apiURL, WebBase: base,
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
		kdecisions.Register(x),
		kchannel.Register(x, kchannel.Options{Queue: q}),
		conns.Register(x),
		kprojects.Register(x),
		executors.Register(x, executors.Options{Owner: owner, Project: kprojects.Get}),
		delegation.Register(x, delegation.Options{Owner: owner, OnError: func(c command.Commit, err error) {
			slog.Error("delegation settle", "object", c.Object, "err", err)
		}}),
		sregistry.Register(x, sregistry.Options{Bundled: bundledPull, Layout: l, Declared: storedName}),
		k.rn.Register(x),
		k.in.Register(x),
		spotcheck.Register(x),
		fm.Register(x),
		rules.RegisterWith(x, ld),
		instances.Register(x, ld),
		blobs.Register(x, l.Blobs),
		kmaterials.RegisterWith(x, ld), // 能力包声明的 material_kinds 与默认写入策略，提交时从加载器读
		kpurge.Register(x, l.Blobs),
		retention.Register(x, l.Blobs, retention.Defaults(cfg.RunRetention, cfg.IngestRetention, cfg.JudgmentRetention)),
		hosted.RegisterWith(x, ld),
		k.fu.Register(),
	); err != nil {
		return nil, err
	}
	att.Attach(x, st, logErr("attention"))
	x.OnCommit(func(command.Commit) { k.sumDue.Store(true) })
	k.hn.Attach(x, st, logErr("health delivery"))
	x.OnCommit(func(command.Commit) { // 推送哪些事（§7.4）：命令提交后立即追赶；不经命令写下的事件由维护循环追赶
		if err := k.push.Sync(context.Background(), st); err != nil {
			slog.Error("push", "err", err)
		}
	})
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
	sources, err := apisources.Mount(st, x, a, k.wh, k.in, apisources.Declared{
		Name: func(s domain.Source) (string, string, bool) {
			zh := declaredName(s, "zh")
			return zh, declaredName(s, "en"), zh != ""
		},
		Sender:     senderInfo(ld),
		SelfSender: k.ids.Fetch,
		CanFetch: func(s domain.Source) bool {
			d, ok := srcdecl.Find(sourceDecls, string(s.Type), s.Command, s.SourceKind)
			return ok && d.FetchesAttachments()
		},
		FetchAttachment: k.ids.FetchAttachment,
		BlobsDir:        l.Blobs,
	})
	if err != nil {
		return nil, err
	}
	ledger, err := schemaLedger()
	if err != nil {
		return nil, err
	}
	if k.srv, err = core.New(core.Deps{Store: st, Auth: a, Web: web.Handler(), Guard: wk.Identity, Groups: []core.RouteGroup{
		apiitems.Routes(x, st, k.stale.After), apidecisions.Routes(x, ""), apiprojects.Routes(st, x), apipolicy.Routes(st, a), apipolicy.ProbeRoutes(st, kdoctor.Env{Model: &model.Client{}, Packs: ld}), apipolicy.OverviewRoutes(st, ld, k.stale.After), creds,
		apicapture.Routes(x), apicontext.Routes(st), apidelegation.Routes(x), apideliveries.Routes(st, q, 0), apichannel.Routes(x, st, nil), apichannel.IdentifyRoutes(kchannel.NewIdentifier(nil)), apiconnections.Routes(st, x, conns, caller, k.send),
		apifollowmsg.Routes(x), apiingest.Routes(k.wh, int64(cfg.MaxWebhookBody)), apipacks.Routes(st, x, ld), apiruns.Routes(st, wk), apievents.Routes(st, 0),
		apiexec.Routes(st, x), apioutbound.Routes(st, x), apipurge.Routes(x), apiretention.Routes(x, st, retention.Defaults(cfg.RunRetention, cfg.IngestRetention, cfg.JudgmentRetention)), sources, apiwhy.Routes(st), apiwhy.RuleChangeRoutes(st, x), apijudgments.Routes(st, &spotcheck.Service{Store: st, Exec: x, Proposer: k.in, Clock: clock.Now}), apihealth.Routes(st, q, ld, cfg.DelegationStallAfter, cfg.UnjudgedRejudgeWindow), apihealth.DoctorRoutes(st, kdoctor.Env{Model: &model.Client{}, Packs: ld, Conns: conns, KeyFile: vault.Path()}), apimaterials.Routes(st, x, hs), apiagentsmd.Routes(x, k.agm), apiversion.Routes(ledger)}}); err != nil {
		return nil, err
	}
	k.clk, err = clock.New(st, k.handleTimer, nil)
	return k, err
}

// defaultSchedules 是开箱默认安排（§7.3）：简报工作日 08:30、复盘每周五 17:30；cron 不带时区，按本机时区解释。
var defaultSchedules = []instances.Default{
	{Pack: "brief", Params: json.RawMessage(`{"schedule":"30 8 * * 1-5"}`)},
	{Pack: "review", Params: json.RawMessage(`{"schedule":"30 17 * * 5"}`)},
}

// handleTimer 把到期 Timer 分派给所属模块；规则与跟进只在事务里排待办，执行要调模型，由后台循环做。
func (k *kernel) handleTimer(tx *store.Tx, c clock.Command) error {
	if err := khealth.RecordClockLag(tx, c, clock.Now()); err != nil { // 迟到只报不修，照常触发
		return err
	}
	switch {
	case k.att.Owns(c):
		return k.att.HandleTimer(tx, c)
	case kdecisions.Owns(c): // 旧模型留下的超时 Timer：只清掉，不代答
		return kdecisions.HandleTimer(tx, c)
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
	if err := switchAuthz(ctx, k.st, clock.Now); err != nil { // 权限模型第二版的一次性切换，幂等（§16.5）
		return err
	}
	// 无法落成 Timer 的 schedule 是某个包的问题，规则状态已登记，不挡启动。
	if err := k.eng.Reconcile(ctx); err != nil {
		slog.Error("rules reconcile", "err", err)
	}
	if err := k.loadHostFiles(ctx); err != nil {
		return err
	}
	// 钥匙逐来源档位的一次性迁移，幂等（§16.5）：在主机配置里的来源登记对齐之后，按它们今天的外发范围换算。
	if err := migrateKeyScopes(ctx, k.st, clock.Now); err != nil {
		return err
	}
	// 升级前为被拒的代理写入挂的单选项问题降级为通知（§14.5、§16.5）；幂等，之后每次启动都找不到要撤的。
	if err := k.st.Update(ctx, func(tx *store.Tx) error {
		ids, err := kdecisions.WithdrawProxyRejectionAsks(tx, clock.Now())
		if len(ids) > 0 {
			slog.Info("withdrew proxy-rejection questions", "items", ids)
		}
		return err
	}); err != nil {
		return err
	}
	if err := instances.Seed(ctx, k.x, k.st, k.packs, owner, defaultSchedules); err != nil {
		return err
	}
	if err := kprojects.SeedCapture(ctx, k.x, k.st, owner); err != nil { // 对话捕获默认开启：已有项目一次性补开（§16.5）
		return err
	}
	if err := k.st.Update(ctx, seedDelivery); err != nil { // 投递矩阵采用建议值：已有部署一次性写入（§16.5）
		return err
	}
	if n, err := sregistry.Backfill(ctx, k.st, storedName, sourceKindNames, clock.Now()); err != nil {
		return err
	} else if n > 0 {
		slog.Info("named sources registered before display names existed", "count", n)
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
	return nil
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
		// 清理单独计时：启动时一次，之后每天 04:10；出错只记日志，不影响下面的维护循环。
		wg.Add(1)
		go func() {
			defer wg.Done()
			k.ret.Run(ctx, sweepHour, sweepMinute, clock.Changed, func(r retention.Report, err error) {
				slog.Info("retention swept", "run_dirs", r.RunDirs, "run_dir_bytes", r.RunDirBytes, "ingest_bodies", r.IngestBodies,
					"judgments", r.Judgments, "skipped_in_flight", len(r.SkippedInFlight))
				if err != nil && ctx.Err() == nil { // 关停时被取消不算错
					slog.Error("retention", "err", err)
				}
			})
		}()
		maintain := func() {
			report("inbound", k.in.Process(ctx))
			report("inbound rejudge", k.in.Rejudge(ctx)) // 模型恢复后重判积压、到期的转 expired（§9.3）
			report("inbound merge", k.in.Sweep(ctx))     // 跨来源合并：攒够一批才调模型（§9.3）
			report("webhook", k.wh.Process(ctx))
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
	wg.Add(1)
	go func() { // 渠道发送器：发送走网络，单独一个循环，不占时钟与后台循环
		defer wg.Done()
		tk := time.NewTicker(senderInterval)
		defer tk.Stop()
		for {
			report("channel sender", k.send.Drain(ctx))
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
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
	report("push", k.push.Sync(ctx, k.st))
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

// seedDelivery 让已有部署一次性采用投递矩阵的建议值（kpolicy.SeedDelivery），操作者是 system（kernel:defaults）。
func seedDelivery(tx *store.Tx) error {
	list, err := instances.List(tx)
	if err != nil {
		return err
	}
	packs := make(map[string]string, len(list))
	for _, in := range list {
		packs[in.ID] = in.Pack
	}
	_, _, err = kpolicy.SeedDelivery(tx, domain.Actor{Type: domain.ActorSystem, Principal: "kernel:defaults", Via: domain.ViaClock, OnBehalfOf: owner}, packs)
	return err
}
