// Package background 是后台运行触发（设计 §13.6、§14.11、附录 C.6）：规则动作 start_background_run 的处理器。
// 命中后依次做前置校验——该事项已有在途运行则跳过并记 Event；链路深度超限只记录不派发；
// 按当前策略复核（规则仍是 auto、权限档位不超过 Web 设置的上限且声明已提供、task 运行者的执行者在项目名单内）
// 不通过则转 needs_input——通过后在同一事务写 Run 和 Event（策略版本、规则 id），再按声明的拉起方式启动：
// 无头运行经外呼调用器拉起（Run 为 queued）；main_session 落到声明了 launch: wake 的 harness 时，Run 写为外部运行（running，
// 不占额度、不起进程），同一事务交唤醒模块产出 class=wake 投递，由网关插件领取。
// 运行结束仍 open、失败或超时，事项转 needs_input，不重试。
//
// 运行参数（runner、harness、access、cwd、executor）只来自规则动作、提示词模板和 Web 设置，不来自触发事件的内容（§14.10）。
// 本包不起子进程也不发网络请求（经 kernel/outcall/proc 入队），不直写其他模块的数据：
// 子事项的委派与阶段转移走命令模型，只写自己的运行关联表，并向 Event 日志追加审计记录。
// 续接（kernel-bg-resume）不在本包；唤醒投递、凭据租约与回执收尾在 kernel/wake。
package background

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/wake"
)

// Action 是本模块处理的规则动作名；装配处以 rules.Options.Handlers[Action] = svc.Handler() 登记。
const Action = "start_background_run"

// 本模块追加的审计记录类型。它们是内部记录，不在对外稳定的事件类型表里（`apa why` 可见）。
const (
	EvLaunched = "background.launched" // 写了 Run 并交给外呼调用器
	EvSkipped  = "background.skipped"  // 该事项已有在途运行，不重复拉起
	EvRefused  = "background.refused"  // 当前策略复核不通过，不拉起
)

// Module 是本模块在存储层登记的迁移：一次运行对应的触发事项、目标事项与会话 id（续接用，§13.6），真理源仍是账本。
// 触发事项与目标事项不同时（runner=task 的目标是子事项）才需要这张表来判断"同一事项只有一个运行"。
var Module = store.Module{Name: "background", Migrations: []store.Migration{{
	Version: 1, Name: "create-run-link",
	SQL: `CREATE TABLE background_run (
	run_id TEXT PRIMARY KEY,
	item TEXT NOT NULL,
	target TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX background_run_item ON background_run(item);`,
}}}

// PackSource 取已加载的能力包（packs.Loader 满足它）。
type PackSource interface {
	Get(name string) (*packs.Pack, bool)
}

// Queue 是外呼调用器的调度入口（*proc.Dispatcher 满足它）。
type Queue interface {
	Dispatch(ctx context.Context) error
}

// Options 配置 Service。Store、Exec、Packs、Queue、RunsDir 必须给出。
type Options struct {
	Store *store.Store
	Exec  *command.Executor
	Packs PackSource
	Queue Queue
	// RunsDir 是运行目录的父目录（$APA_HOME/runs），每次运行一个子目录。
	RunsDir string
	// APIURL 经 APA_URL 交给运行者的 CLI。
	APIURL string
	// Owner 是本人的标识，作为内核代为转 needs_input 时的 on_behalf_of。
	Owner string
	// Host 是 APA 主机名，用来判断项目在本机有没有检出；空时取 os.Hostname()，取不到按没有本机检出拒绝。
	Host string
	Now  func() time.Time
	// DefaultTimeout 在模板未声明 timeout 时使用，默认 30 分钟。
	DefaultTimeout time.Duration
	// MaxChainDepth 是链路深度阈值，默认与命令管线一致（§14.9）。
	MaxChainDepth int
	// Wake 产出唤醒投递；为空时声明了 launch: wake 的 harness 不拉起，转 needs_input。
	Wake *wake.Service
	// OnError 接收 Settle 里无法交给调用方的错误（落账失败等）；可为空。
	OnError func(runID string, err error)
}

// Service 是后台运行入口，可并发使用。
type Service struct{ o Options }

// New 返回 Service。
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Exec == nil || o.Packs == nil || o.Queue == nil || o.RunsDir == "" {
		return nil, errors.New("background: Store, Exec, Packs, Queue and RunsDir are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.DefaultTimeout <= 0 {
		o.DefaultTimeout = 30 * time.Minute
	}
	if o.MaxChainDepth <= 0 {
		o.MaxChainDepth = command.DefaultMaxChainDepth
	}
	if o.Owner == "" {
		o.Owner = "owner"
	}
	if o.Host == "" {
		o.Host, _ = os.Hostname()
	}
	return &Service{o}, nil
}

const systemPrincipal = "kernel:background"

// system 是内核在运行结束后替后台运行转 needs_input 的内部身份：目标事项不一定指派给它（main_session 的目标是规则选中的事项本身），
// 所以用 delegate 级并写明代表本人，授权依据与规则动作一致。
var system = domain.Actor{Type: domain.ActorSystem, Principal: systemPrincipal, Via: domain.ViaClock}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the rule or the prompt template")
}

var tierRank = map[string]int{policy.PermRead: 1, policy.PermEdit: 2, policy.PermFull: 3}

// withinCap：档位不超过本人在 Web 按 harness 设的上限，未按 harness 设置的用默认上限（§13.6）。
func withinCap(pol policy.Policy, harness, tier string) bool {
	limit := pol.PermissionCap.ByHarness[harness]
	if limit == "" {
		limit = pol.PermissionCap.Default
	}
	return tierRank[tier] != 0 && tierRank[tier] <= tierRank[limit]
}

func str(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// spec 是一次拉起的全部参数：来自规则动作、提示词模板和 Web 设置，没有任何一项来自事件内容。
type spec struct {
	runner, access, cwd, harness, executor string
	prompt                                 packs.Prompt
	timeout                                time.Duration
}

func (s *Service) parse(c rules.Call) (spec, error) {
	pack, _, _ := strings.Cut(c.Rule, "/")
	tmpl := str(c.Action, "prompt")
	pk, ok := s.o.Packs.Get(pack)
	if !ok {
		return spec{}, invalid("pack " + pack + " is not loaded")
	}
	pr, ok := pk.Prompts[tmpl]
	if !ok {
		return spec{}, invalid("prompt " + pack + "/" + tmpl + " does not exist")
	}
	sp := spec{runner: str(c.Action, "runner"), access: str(c.Action, "access"), cwd: str(c.Action, "cwd"),
		harness: str(c.Action, "harness"), executor: str(c.Action, "executor"), prompt: pr, timeout: s.o.DefaultTimeout}
	switch {
	case sp.runner == "task" && pr.Meta.Agent != "task":
		return sp, invalid("runner task needs a prompt with agent: task, got " + pack + "/" + tmpl)
	case sp.runner == "main_session" && pr.Meta.RunAs != "main_session":
		return sp, invalid("runner main_session needs a prompt with run_as: main_session, got " + pack + "/" + tmpl)
	case sp.runner != "task" && sp.runner != "main_session":
		return sp, invalid("start_background_run runner must be task or main_session")
	case sp.runner == "task" && sp.executor == "":
		return sp, invalid("runner task needs an executor")
	}
	if sp.access == "" {
		sp.access = pr.Meta.Access
	}
	if sp.access == "" {
		sp.access = policy.PermRead
	}
	if tierRank[sp.access] == 0 {
		return sp, invalid("access must be read, edit or full")
	}
	if sp.cwd == "" {
		sp.cwd = pr.Meta.Cwd
	}
	if sp.cwd == "" {
		sp.cwd = "run_dir"
	}
	if pr.Meta.Timeout != "" {
		d, err := time.ParseDuration(pr.Meta.Timeout)
		if err != nil || d <= 0 {
			return sp, invalid("timeout " + pr.Meta.Timeout + " is not a positive duration")
		}
		sp.timeout = d
	}
	return sp, nil
}

// facts 是拉起前一次性读出的账本事实。
type facts struct {
	pv       int
	pol      policy.Policy
	mode     string
	item     domain.Item
	busy     bool
	project  *domain.Project
	rec      executors.Record
	declared bool
	depth    int // 本次拉起的链路深度：触发事件的深度加一（规则拉起新运行，§14.9）
}

func (s *Service) read(ctx context.Context, c rules.Call, sp spec) (f facts, err error) {
	err = s.o.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		if f.pv, f.pol, err = policy.Current(tx); err != nil {
			return err
		}
		sts, err := rules.List(tx)
		if err != nil {
			return err
		}
		for _, st := range sts {
			if st.Key == c.Rule {
				f.mode = st.Mode
			}
		}
		if f.item, err = items.Get(tx, c.Item.ID); err != nil {
			return err
		}
		if f.busy, err = InFlight(tx, f.item.ID); err != nil {
			return err
		}
		if f.item.Project != "" {
			p, err := projects.Get(tx, f.item.Project)
			if err != nil {
				return err
			}
			f.project = &p
		}
		if sp.harness == "" {
			sp.harness = f.pol.DefaultHarness
		}
		if sp.harness == "" {
			return nil
		}
		recs, err := executors.Records(tx, executors.StateActive)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.Harness == sp.harness && r.Declaration.Headless != nil {
				f.rec, f.declared = r, true
			}
		}
		return nil
	})
	return f, err
}

// InFlight：事项自己或由它启动的运行目标（task 运行者的子事项）有排队或运行中的 Run（排队中的算在途，§7.7）。
// 拉起前的"同一事项只有一个运行"与 `apa context` 的在途运行提示用同一口径。
func InFlight(tx *store.Tx, item string) (bool, error) {
	if b, err := proc.InFlight(tx, item); err != nil || b {
		return b, err
	}
	r, err := tx.Query(`SELECT target FROM background_run WHERE item = ?`, item)
	if err != nil {
		return false, err
	}
	var targets []string
	for r.Next() {
		var t string
		if err := r.Scan(&t); err != nil {
			r.Close()
			return false, err
		}
		targets = append(targets, t)
	}
	if err := r.Close(); err != nil {
		return false, err
	}
	for _, t := range targets {
		if b, err := proc.InFlight(tx, t); err != nil || b {
			return b, err
		}
	}
	return false, nil
}

// Handler 返回 start_background_run 动作的处理器。
func (s *Service) Handler() rules.Handler { return s.start }

func (s *Service) start(ctx context.Context, c rules.Call) error {
	if c.Item == nil {
		return invalid("start_background_run needs a triggering item, but this rule has none")
	}
	sp, err := s.parse(c)
	if err != nil {
		return err
	}
	f, err := s.read(ctx, c, sp)
	if err != nil {
		return err
	}
	f.depth = chain.Next(c.Depth)
	if f.busy {
		return s.audit(ctx, c, f, EvSkipped, f.item.ID, "该事项已有在途运行，不重复拉起", nil)
	}
	if f.depth > s.o.MaxChainDepth { // 与命令管线同一口径：超过阈值只记录不派发，健康类投递由 kernel/health 据该事件统一产出
		return chain.Record(ctx, s.o.Store, s.o.Now(), c.Rule, f.item.ID, f.depth, s.o.MaxChainDepth)
	}
	harness := sp.harness
	if harness == "" {
		harness = f.pol.DefaultHarness
	}
	headless, ms := executors.Headless{}, executors.MainSession{}
	if f.declared {
		headless = *f.rec.Declaration.Headless
		if m := f.rec.Declaration.MainSession; m != nil {
			ms = *m
		}
	}
	workDir, noCheckout := projects.WorkDir(sp.cwd, f.project, s.o.Host)
	wakes := sp.runner == "main_session" && ms.Launch == runidentity.LaunchWake // 拉起方式由声明决定，二者择一（§13.6）
	var reason string
	switch {
	case f.mode != rules.ModeAuto:
		reason = "规则已不是 auto"
	case harness == "":
		reason = "规则没有指定 harness，Web 设置里也没有默认 harness"
	case !f.declared:
		reason = "没有已确认的无头运行声明：" + harness
	case wakes && s.o.Wake == nil:
		reason = "harness " + harness + " 声明了唤醒真实主会话，但内核没有接入唤醒"
	case headless.Access[sp.access] == nil:
		reason = "无头运行声明未提供权限档位：" + sp.access
	case !withinCap(f.pol, harness, sp.access):
		reason = "权限档位 " + sp.access + " 超过 Web 设置的上限"
	case sp.runner == "task" && (f.project != nil && !f.project.ExecutorAllowed(sp.executor, s.o.Owner)):
		reason = "执行者 " + sp.executor + " 不在项目允许的执行者名单内"
	case noCheckout != "":
		reason = noCheckout
	}
	if reason != "" {
		return s.refuse(ctx, c, f, reason)
	}

	target := f.item.ID
	if sp.runner == "task" { // 内核先把子事项委派给执行者，运行目标是它（§13.6）
		if target, err = s.createChild(c, f.item, sp.executor); err != nil {
			return err
		}
	}
	return s.launch(ctx, c, f, sp, harness, headless, ms, wakes, target, workDir)
}

// createChild 经 delegate.create 建委派子事项（与 `apa delegate` 同一路径：项目名单校验、父事项转 waiting），
// 运行者因此能按委派回执写回它。
func (s *Service) createChild(c rules.Call, parent domain.Item, executor string) (string, error) {
	res, err := c.Do("delegate.create", map[string]any{"parent": parent.ID, "assignee": executor,
		"reason": "rule " + c.Rule + ": background task runner"})
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("background: delegate.create returned no id (suppressed=%v): %v", res.Suppressed, err)
	}
	return out.ID, nil
}

func newRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(b[:]), nil
}

func expand(in []string, pairs ...string) []string {
	r := strings.NewReplacer(pairs...)
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = r.Replace(a)
	}
	return out
}

// renderPrompt 只代入内核给出的 id 与目录（C.7 的固定占位符），事件内容无从进入。
func renderPrompt(body, item, run, project string) string {
	pairs := []string{}
	for k, v := range map[string]string{"item_id": item, "run_id": run, "project_id": project} {
		pairs = append(pairs, "{"+k+"}", v, "{ "+k+" }", v)
	}
	return strings.NewReplacer(pairs...).Replace(body)
}

func (s *Service) launch(ctx context.Context, c rules.Call, f facts, sp spec, harness string, hl executors.Headless, ms executors.MainSession, wakes bool, target, workDir string) error {
	runID, err := newRunID()
	if err != nil {
		return err
	}
	basis := &domain.Basis{PolicyVersion: f.pv, RuleID: c.Rule}
	run := domain.Run{ID: runID, Category: domain.RunBackground, Runner: sp.runner, Launch: "headless", Target: target,
		Harness: harness, Access: sp.access, Basis: basis, ChainDepth: f.depth, TimeoutSec: int(sp.timeout / time.Second)}
	if wakes { // 进程在真实主会话所在处：不写运行目录与命令，凭据由网关插件换取（§13.6）
		run.Launch = runidentity.LaunchWake
		return s.enqueue(ctx, c, f, proc.Request{Run: run, External: true}, func(tx *store.Tx) error {
			_, err := s.o.Wake.Open(tx, runID)
			return err
		})
	}
	runDir := filepath.Join(s.o.RunsDir, runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	prompt := renderPrompt(sp.prompt.Body, target, runID, f.item.Project)
	if err := os.WriteFile(filepath.Join(runDir, "prompt.md"), []byte(prompt), 0o600); err != nil {
		return err
	}
	dir := runDir
	if workDir != "" {
		dir = workDir
	}
	argv := expand(hl.Command, "{prompt}", prompt, "{run_dir}", runDir)
	argv = append(argv, hl.Output...)
	if sp.runner == "main_session" {
		argv = append(argv, ms.Args...)
	}
	argv = append(argv, hl.Access[sp.access]...)
	if len(hl.Session.Assign) > 0 {
		run.SessionID = runID
		argv = append(argv, expand(hl.Session.Assign, "{session_id}", run.SessionID)...)
	}
	return s.enqueue(ctx, c, f, proc.Request{Run: run, Spec: proc.Spec{Command: argv, Dir: dir}}, nil)
}

// enqueue 在一个事务里写 Run、运行关联与 launched 审计记录（与 also 一起提交），再交调度器。
func (s *Service) enqueue(ctx context.Context, c rules.Call, f facts, req proc.Request, also func(*store.Tx) error) error {
	run := req.Run
	err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := proc.Enqueue(tx, req, s.o.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO background_run (run_id, item, target) VALUES (?, ?, ?)`, run.ID, f.item.ID, run.Target); err != nil {
			return err
		}
		if also != nil {
			if err := also(tx); err != nil {
				return err
			}
		}
		body, _ := json.Marshal(map[string]any{"run_id": run.ID, "rule": c.Rule, "runner": run.Runner, "launch": run.Launch,
			"harness": run.Harness, "access": run.Access, "target": run.Target})
		_, err := eventlog.Append(tx, domain.Event{Type: EvLaunched, Object: f.item.ID, Actor: ruleActor(c.Rule), Basis: run.Basis,
			Reason: "rule " + c.Rule + ": background run", Payload: body, ChainDepth: f.depth, At: s.o.Now()})
		return err
	})
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.CodeConflict { // 并发下别处刚入队：按在途跳过
			return s.audit(ctx, c, f, EvSkipped, f.item.ID, "该事项已有在途运行，不重复拉起", nil)
		}
		return err
	}
	return s.o.Queue.Dispatch(ctx)
}

func ruleActor(rule string) domain.Actor {
	return domain.Actor{Type: domain.ActorAgent, Principal: "rule:" + rule, Via: domain.ViaRule}
}

// audit 追加一条带策略版本和规则 id 的审计记录。
func (s *Service) audit(ctx context.Context, c rules.Call, f facts, typ, object, reason string, extra map[string]any) error {
	p := map[string]any{"rule": c.Rule, "reason": reason}
	for k, v := range extra {
		p[k] = v
	}
	body, _ := json.Marshal(p)
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: typ, Object: object, Actor: ruleActor(c.Rule),
			Basis: &domain.Basis{PolicyVersion: f.pv, RuleID: c.Rule}, Reason: reason, Payload: body,
			ChainDepth: f.depth, At: s.o.Now()})
		return err
	})
}

// refuse 记拒绝记录，并把触发事项转 needs_input 交本人：附原因，选项含重新运行、改派、放弃（§8.3、§13.6）。
func (s *Service) refuse(ctx context.Context, c rules.Call, f facts, reason string) error {
	if err := s.audit(ctx, c, f, EvRefused, f.item.ID, reason, nil); err != nil {
		return err
	}
	if f.item.Phase == domain.PhaseNeedsInput {
		return nil
	}
	if _, err := c.Do("item.update", map[string]any{"id": f.item.ID, "ask": askFor(reason), "reason": reason}); err != nil {
		return err
	}
	_, err := c.Do("item.transition", map[string]any{"id": f.item.ID, "to": domain.PhaseNeedsInput, "reason": reason})
	return err
}

func askFor(reason string) map[string]any {
	return map[string]any{"question": "后台运行未完成：" + reason, "options": []domain.AskOption{
		{ID: "rerun", Label: "重新运行"}, {ID: "reassign", Label: "改派"}, {ID: "drop", Label: "放弃", Next: domain.PhaseDropped}}}
}

// Env 供 proc.Options.Env 使用：运行启动时为后台运行签发运行凭据并注入环境变量，凭据明文不落盘、不入账本。
// 重新签发会替换旧凭据，所以重启后重新执行的排队运行也能拿到有效凭据。签发失败返回 nil：运行者没有凭据，
// 回执缺失，按失败处理，而不是带着空凭据假装成功。
func (s *Service) Env(r domain.Run) map[string]string {
	if r.Category != domain.RunBackground {
		return nil
	}
	var env map[string]string
	err := s.o.Store.Update(context.Background(), func(tx *store.Tx) error {
		secret, err := runidentity.Issue(tx, r.ID, s.o.Now())
		if err != nil {
			return err
		}
		it, err := items.Get(tx, r.Target)
		if err != nil {
			return err
		}
		env = runidentity.Env(s.o.APIURL, secret, r, it.Project)
		return nil
	})
	if err != nil {
		s.fail(r.ID, fmt.Errorf("issue run credential: %w", err))
		return nil
	}
	return env
}

func (s *Service) fail(runID string, err error) {
	if s.o.OnError != nil {
		s.o.OnError(runID, err)
	}
}

// Recovered 处理内核重启时无存活进程、已被 proc.Dispatcher.Recover 按失败落账的运行（§13.6）。
func (s *Service) Recovered(ctx context.Context, failed []domain.Run) {
	for _, r := range failed {
		s.Settle(ctx, proc.Result{Run: r, Reason: "no live process after restart"})
	}
}

// sessionOf 取 stdout 末段结构化摘要里的会话 id；没有摘要返回空。
func sessionOf(stdout []byte) string {
	lines := strings.Split(strings.TrimRight(string(stdout), "\r\n \t"), "\n")
	var sm struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &sm) != nil {
		return ""
	}
	return sm.SessionID
}

// Settle 在运行结束后收尾（供 proc.Options.OnResult 调用；非后台运行忽略）。
// 失败、超时，或运行结束时目标事项仍是 open，都转 needs_input，不重试（§13.6）；
// 目标事项已离开 open（done、waiting、运行者自己提了问题）则保留运行者写下的状态。
func (s *Service) Settle(ctx context.Context, res proc.Result) {
	run := res.Run
	if run.Category != domain.RunBackground {
		return
	}
	if err := s.settle(ctx, res); err != nil {
		s.fail(run.ID, err)
	}
}

func (s *Service) settle(ctx context.Context, res proc.Result) error {
	run := res.Run
	var it domain.Item
	if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { it, err = items.Get(tx, run.Target); return }); err != nil {
		return err
	}
	failure := ""
	switch {
	case run.State != domain.RunSucceeded:
		failure = "运行" + string(run.State) + "：" + res.Reason
	case it.Phase == domain.PhaseOpen:
		failure = "运行结束，事项仍是 open，没有回执"
	}
	if failure != "" && it.Phase != domain.PhaseNeedsInput && !it.Phase.Terminal() { // 运行者自己提了问题就保留它的问题
		if err := s.needsInput(ctx, it.ID, failure); err != nil {
			return err
		}
	}
	return s.record(ctx, run, it.ID, sessionOf(res.Stdout), failure)
}

func (s *Service) needsInput(ctx context.Context, itemID, reason string) error {
	for _, step := range []struct {
		cmd string
		in  map[string]any
	}{
		{"item.update", map[string]any{"id": itemID, "ask": askFor(reason), "reason": "background run failed"}},
		{"item.transition", map[string]any{"id": itemID, "to": domain.PhaseNeedsInput, "reason": "background run: " + reason}},
	} {
		b, _ := json.Marshal(step.in)
		a := system
		a.OnBehalfOf = s.o.Owner
		_, err := s.o.Exec.Do(ctx, command.Request{Identity: command.Trusted{Actor: a, Level: authz.LevelDelegate},
			Command: step.cmd, Input: b, Evidence: "background run " + itemID, Owner: s.o.Owner})
		if err != nil {
			return err
		}
	}
	return nil
}

// record 记会话 id（续接用），并追加一条 actor=agent 的 Event。
func (s *Service) record(ctx context.Context, run domain.Run, itemID, sessionID, failure string) error {
	if sessionID == "" {
		sessionID = run.SessionID
	}
	state := "succeeded"
	if failure != "" {
		state = "failed"
	}
	body, err := json.Marshal(map[string]any{"run_id": run.ID, "category": "background", "runner": run.Runner, "state": state,
		"session_id": sessionID, "error": failure})
	if err != nil {
		return err
	}
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE background_run SET session_id = ? WHERE run_id = ?`, sessionID, run.ID); err != nil {
			return err
		}
		depth, err := items.ChainDepth(tx, itemID) // 沿用事项当前深度：运行者的写入已在运行深度上加一，结束记录不能把它拉回去
		if err != nil {
			return err
		}
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvRunFinished, Object: itemID,
			Actor:  domain.Actor{Type: domain.ActorAgent, Principal: "agent:background", Via: domain.ViaClock, Run: run.ID, Harness: run.Harness},
			Reason: "background " + state, Payload: body, ChainDepth: depth, Basis: run.Basis, At: s.o.Now()})
		return err
	})
}

// SessionOf 返回某事项最近一次后台运行记下的会话 id（触发事项或目标事项均可）；没有则 ok 为假。
func SessionOf(tx *store.Tx, itemID string) (sessionID string, ok bool, err error) {
	r, err := tx.Query(`SELECT session_id FROM background_run WHERE (item = ? OR target = ?) AND session_id != '' ORDER BY rowid DESC LIMIT 1`, itemID, itemID)
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	if !r.Next() {
		return "", false, r.Err()
	}
	if err := r.Scan(&sessionID); err != nil {
		return "", false, err
	}
	return sessionID, true, nil
}

// Link 是某事项最近一次后台运行的关联：哪次运行、由哪个事项触发、会话 id（续接模块读它，§13.6）。
type Link struct{ RunID, Item, SessionID string }

// LastLink 返回某事项（触发事项或目标事项均可）最近一次后台运行的关联；没有则 ok 为假。
func LastLink(tx *store.Tx, itemID string) (l Link, ok bool, err error) {
	r, err := tx.Query(`SELECT run_id, item, session_id FROM background_run WHERE item = ? OR target = ? ORDER BY rowid DESC LIMIT 1`, itemID, itemID)
	if err != nil {
		return Link{}, false, err
	}
	defer r.Close()
	if !r.Next() {
		return Link{}, false, r.Err()
	}
	if err := r.Scan(&l.RunID, &l.Item, &l.SessionID); err != nil {
		return Link{}, false, err
	}
	return l, true, nil
}
