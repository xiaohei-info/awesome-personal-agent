// Package command 是所有写入的统一路径（设计 §7.0）：认证 → 授权 → 校验 → 事务 → 提交后。
// 本包只提供管线与登记表，不含任何业务命令；业务模块登记 Spec，经 Do 执行，不直写对方的数据。
//
// 管线保证：乐观版本与幂等键、链路深度由内核继承（调用方只能指明"继承自哪个对象"，写不了数值，
// 输入里出现内核维护字段直接拒绝）、被拒绝的高风险命令在独立事务里留痕。
package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// DefaultMaxChainDepth 是链路深度阈值（§14.9）。
const DefaultMaxChainDepth = 8

// Module 是本模块在存储层登记的迁移：对象乐观版本与幂等结果。
var Module = store.Module{Name: "command", Migrations: []store.Migration{{
	Version: 1, Name: "create-version-and-idempotency",
	SQL: `CREATE TABLE command_version (
	object TEXT PRIMARY KEY,
	version INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE command_idem (
	principal TEXT NOT NULL,
	command TEXT NOT NULL,
	key TEXT NOT NULL,
	input_hash TEXT NOT NULL,
	result TEXT NOT NULL,
	version INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (principal, command, key)
) WITHOUT ROWID;`,
}}}

// ---- 第 1 步：认证 ----

// Caller 是认证的结果：操作者与信任级。
type Caller struct {
	Actor domain.Actor
	Level authz.Level
	Kind  auth.Kind // 仅凭据入口有值
	// Credential 是凭据 id，仅凭据入口有值；授权单按它认人（Actor.Principal 在绑定执行者时是执行者 id）。
	Credential string
	// RunScope 只有运行凭据有值：据账本判断授权对象是否在运行范围内（目标事项及其子事项 / 目标事项所属项目）。
	// 管线用它填 authz.Object 的 InRunScope、InRunProject，命令自己不必也无法声明。
	RunScope func(tx *store.Tx, o authz.Object) (inScope, inProject bool, err error)
	// RunDepth 只有运行凭据有值：运行记录的链路深度，本运行发出的命令据它继承（§14.9）。
	RunDepth int
}

// Identity 在事务内把请求携带的凭据确定为 Caller。
type Identity interface {
	Authenticate(tx *store.Tx) (Caller, error)
}

// Bearer 是长期凭据入口（token、ext 密钥、webhook 密钥）。
type Bearer struct {
	Auth   *auth.Auth
	Secret string
	Origin auth.Origin
	// OnBehalfOf 是传输层为 delegate 凭据声明的本人：Bearer 认证只知道凭据，不知道替谁说话。
	// 管线只在第二档（替本人做决定，§14.5）保留它；第一档是 agent 自己的工作，Event 不带。
	OnBehalfOf string
}

func (b Bearer) Authenticate(tx *store.Tx) (Caller, error) {
	c, err := b.Auth.Resolve(tx, b.Secret)
	if err != nil {
		return Caller{}, err
	}
	l := authz.LevelWorker
	switch c.Kind {
	case auth.Observer:
		l = authz.LevelObserver
	case auth.Delegate:
		l = authz.LevelDelegate
	}
	a := c.Actor(b.Origin)
	if l == authz.LevelDelegate {
		a.OnBehalfOf = b.OnBehalfOf
	}
	return Caller{Actor: a, Level: l, Kind: c.Kind, Credential: c.ID}, nil
}

// Trusted 是内核内部与已由其他模块核实的入口：Web 本人会话、运行凭据、时钟、规则。
// 构造者负责先完成它自己的校验；本包只拒绝没有操作者的情况。
type Trusted Caller

func (t Trusted) Authenticate(*store.Tx) (Caller, error) {
	if t.Actor.Principal == "" {
		return Caller{}, domain.NewError(domain.CodeUnauthenticated, "caller has no principal", "authenticate and retry")
	}
	return Caller(t), nil
}

// ---- 链路深度（§14.9） ----

// ChainMode 说明本命令如何得到链路深度。
type ChainMode int

const (
	ChainAuto    ChainMode = iota // 零值：调用方未指明，由管线按调用者与命令推定（resolveDepth）
	ChainInherit                  // 规则动作：继承来源对象并加一
	ChainKeep                     // 提问、定时、挂靠消息：继承不加
)

// Chain 指明深度来源对象；深度数值只由内核从账本里该对象最近一条事件读取。
// 对外入口不填它，深度由 resolveDepth 按调用者与命令推定。
type Chain struct {
	Mode ChainMode
	From string
}

// ---- 请求与登记 ----

// Request 是一次命令调用。Input 是调用方可写的 JSON；Chain 只给进程内的调用方（规则、时钟、回执处理），
// 对外传输层不应把它暴露给请求体。
type Request struct {
	Identity      Identity
	Command       string
	Input         json.RawMessage
	IdemKey       string // 空表示不要求幂等
	ExpectVersion *int64 // 空表示不检查
	Chain         Chain
	RuleID        string // 自动动作（规则）的授权依据
	Evidence      string
	Grant         string // 第二档的另一条依据：授权单 id；与 Evidence 不能同时携带
	Owner         string
	WebURL        string
}

// Ctx 是 Apply 的上下文。
type Ctx struct {
	Tx         *store.Tx
	Caller     Caller
	Input      json.RawMessage
	Object     authz.Object
	ChainDepth int
	Basis      *domain.Basis
	Grant      string // 本次放行所依据的授权单 id；只有第二档且依据授权单时非空，Emit 的每条事件都记它
	Evidence   string // 请求携带的本人原话（第二档已由授权校验过存在；会话引用在 Caller.Actor.Session）
	now        func() time.Time
	seqs       []int64
	events     []domain.Event
}

// Now 返回本次命令的时刻，业务写入的时间戳取它；一条命令内恒定，所以与 Emit 缺省的事件时间相同。
func (c *Ctx) Now() time.Time { return c.now() }

// Events 返回本次命令到目前为止追加的事件（含序号），供同事务回调识别发生了什么。
func (c *Ctx) Events() []domain.Event { return c.events }

// Emit 在同一事务内追加事件。Actor、ChainDepth、Basis 一律由内核覆盖，调用方写不了。
func (c *Ctx) Emit(e domain.Event) (int64, error) {
	e.Actor, e.ChainDepth, e.Basis = c.Caller.Actor, c.ChainDepth, c.Basis
	if c.Grant != "" {
		e.Grant = c.Grant
	}
	if e.At.IsZero() {
		e.At = c.now()
	}
	seq, err := eventlog.Append(c.Tx, e)
	if err == nil {
		e.Seq = seq
		c.seqs = append(c.seqs, seq)
		c.events = append(c.events, e)
	}
	return seq, err
}

// Spec 登记一个命令。
type Spec struct {
	Name   string
	Action authz.Action
	// Actions 非空时取代 Action：依调用者与对象给出需要逐一通过的授权动作（如"改自己的事项"加"校验执行者"）；
	// 第一个被拒绝的动作决定结果。
	Actions func(tx *store.Tx, c Caller, o authz.Object, in json.RawMessage) []authz.Action
	// Object 在事务内据账本解析授权对象；对象的所有字段来自账本，不来自 Input。
	Object func(tx *store.Tx, in json.RawMessage) (authz.Object, error)
	// Validate 校验 kind 的 spec schema、阶段进入约束等；可为空。
	Validate func(tx *store.Tx, o authz.Object, in json.RawMessage) error
	// Apply 更新投影并经 Emit 追加事件；返回值序列化为结果，幂等重放时原样返回。
	Apply func(c *Ctx) (any, error)
	// ChannelAnswer 非空时（ActionChannelAnswer），管线用返回的渠道与回答人填授权请求以查回答人白名单，
	// webURL 随 web_only 拒绝返回；渠道与回答人只能据账本与凭据得出，不来自对象之外的声明。
	ChannelAnswer func(tx *store.Tx, c Caller, in json.RawMessage) (channel, sender, webURL string, err error)
	// Tier 声明命令的代理写档位（§14.5、附录 B.1）；零值不是代理写命令。判定只在管线一处，命令体内不重复。
	Tier authz.Tier
	// Bound 非空时，delegate 调用满足它则按第一档（agent 自己的工作）：对象绑定只据账本与凭据得出。
	// 不满足时按 Tier。Tier 为第二档的命令用它声明"绑定时降到第一档"。
	Bound func(tx *store.Tx, c Caller, o authz.Object, in json.RawMessage) (bool, error)
	// Receipt 标记回执与捕获类命令（§14.5 失败可见）：它们多由插件异步发起、对模型不可见，
	// 内核可达但拒绝（退出码 2、4、23）时，管线在独立事务里另记 health.alert。
	Receipt bool
	// RequireVersion 为真时必须带 ExpectVersion。
	RequireVersion bool
	// Kinds 是允许调用的 ext/webhook 凭据种类；默认这两类凭据不能调用。
	Kinds []auth.Kind
}

// Result 是命令结果。
type Result struct {
	Value      json.RawMessage
	Version    int64 // 对象写入后的版本；无对象为 0
	Seqs       []int64
	Replayed   bool // 幂等键命中，返回首次结果，未重复写入
	Suppressed bool // 链路深度超阈值：只记录不派发
}

// Commit 在提交后交给内部消费者（事件规则、触达、订阅等待方）。
type Commit struct {
	Command string
	Actor   domain.Actor
	Object  string
	Seqs    []int64
	Input   json.RawMessage // 命令输入与结果：没有对象 id 的命令（新建、按仓库检查）靠它定位
	Value   json.RawMessage
}

// ReceiptKind 是回执与捕获被拒绝的 health.alert 的 payload.kind。
const ReceiptKind = "receipt"

// Rejection 是一次被拒绝的命令，交给 OnReject 回调在记 command.rejected 的同一事务里处理。
type Rejection struct {
	Command string
	Actor   domain.Actor
	Object  string // 授权对象的 id；新建类命令没有对象为空
	Exit    int
	Why     string
	At      time.Time
}

// Executor 持有登记表与存储。
type Executor struct {
	st            *store.Store
	now           func() time.Time
	MaxChainDepth int

	mu    sync.RWMutex
	specs map[string]Spec
	subs  []func(Commit)
	inTx  []func(*Ctx) error
	onRej []func(*store.Tx, Rejection) error
}

// New 返回 Executor；now 为 nil 时用系统时钟。
func New(st *store.Store, now func() time.Time) *Executor {
	if now == nil {
		now = clock.Now
	}
	return &Executor{st: st, now: now, MaxChainDepth: DefaultMaxChainDepth, specs: map[string]Spec{}}
}

// Register 登记命令；名称重复或缺 Apply/Object 会被拒绝。
func (x *Executor) Register(s Spec) error {
	if s.Name == "" || s.Apply == nil || s.Object == nil {
		return fmt.Errorf("command: spec %q needs name, Object and Apply", s.Name)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.specs[s.Name]; ok {
		return fmt.Errorf("command: %q already registered", s.Name)
	}
	x.specs[s.Name] = s
	return nil
}

// Tier 返回已登记命令声明的代理写档位，供契约测试对照附录 B.1 的清单。
func (x *Executor) Tier(name string) (authz.Tier, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	s, ok := x.specs[name]
	return s.Tier, ok
}

// Store 返回执行器所用的账本，供只持有执行器的路由读取。
func (x *Executor) Store() *store.Store { return x.st }

// OnCommit 登记提交后的消费者；按登记顺序同步调用，重放与被拒绝的命令不触发。
func (x *Executor) OnCommit(f func(Commit)) {
	x.mu.Lock()
	x.subs = append(x.subs, f)
	x.mu.Unlock()
}

// InTx 登记同事务回调：命令的 Apply 成功后、提交前，按登记顺序在同一写事务里调用；
// 返回错误则整个命令回滚。供需要与命令原子落账的模块使用（如回答与续接 Run 入队）；
// 链路超限只记录不派发的命令、重放与被拒绝的命令不触发。回调不得另开事务，也不得发起外呼。
func (x *Executor) InTx(f func(*Ctx) error) {
	x.mu.Lock()
	x.inTx = append(x.inTx, f)
	x.mu.Unlock()
}

// OnReject 登记拒绝回调：被拒绝的命令（退出码 20、21、22、23 与运行身份被拒绝的决策命令）在记 command.rejected 之后、
// 同一事务里按登记顺序调用，返回错误则拒绝留痕一并回滚。供事项转 needs_input 等"依据不足不静默"的处理（§14.5）。
// 回调不得另开事务，也不得发起外呼。
func (x *Executor) OnReject(f func(*store.Tx, Rejection) error) {
	x.mu.Lock()
	x.onRej = append(x.onRej, f)
	x.mu.Unlock()
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

// 内核维护字段：任何层级出现都拒绝。ext 与 spec 子树是用户数据，不扫描。
var kernelFields = []string{"chain_depth", "version", "seq", "unattended", "risk_class", "screening_pending", "basis"}

// foldIn 与 encoding/json 的键匹配一致（大小写不敏感，含 Kelvin 符号等 Unicode 折叠），否则变体键能绕过防写。
func foldIn(k string, set ...string) bool {
	for _, f := range set {
		if strings.EqualFold(k, f) {
			return true
		}
	}
	return false
}

func scanKernelFields(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if foldIn(k, kernelFields...) {
				return k
			}
			if foldIn(k, "ext", "spec") {
				continue
			}
			if f := scanKernelFields(c); f != "" {
				return f
			}
		}
	case []any:
		for _, c := range t {
			if f := scanKernelFields(c); f != "" {
				return f
			}
		}
	}
	return ""
}

func checkInput(in json.RawMessage) error {
	if len(bytes.TrimSpace(in)) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return invalid("input is not valid JSON")
	}
	if f := scanKernelFields(v); f != "" {
		return invalid("field " + f + " is maintained by the kernel and cannot be written")
	}
	return nil
}

// rejected 携带需要在独立事务里留痕的拒绝事件。
type rejected struct {
	err error
	ev  domain.Event
}

// trace 记下管线走到哪一步得到的操作者与对象，供失败后的留痕使用。
type trace struct {
	actor domain.Actor
	obj   string
}

func exitOf(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return domain.ExitUnclassified
}

func (r *rejected) Error() string { return r.err.Error() }
func (r *rejected) Unwrap() error { return r.err }

// Do 执行一次命令：五步在同一写事务里完成（提交后消费者除外）。失败整体回滚；
// 退出码 20/21/22 与运行身份被拒绝的决策命令另开事务记 command.rejected。
func (x *Executor) Do(ctx context.Context, req Request) (Result, error) {
	x.mu.RLock()
	spec, ok := x.specs[req.Command]
	x.mu.RUnlock()
	if !ok {
		return Result{}, invalid("unknown command " + req.Command)
	}
	if req.Identity == nil {
		return Result{}, domain.NewError(domain.CodeUnauthenticated, "no credential", "authenticate and retry")
	}
	var (
		res    Result
		commit Commit
		tr     trace
	)
	err := x.st.Update(ctx, func(tx *store.Tx) error {
		var err error
		res, commit, err = x.pipeline(tx, spec, req, &tr)
		return err
	})
	if err != nil {
		rj, isRej := err.(*rejected)
		orig := err
		if isRej {
			orig = rj.err
		}
		code := exitOf(orig)
		alert := spec.Receipt && (code == domain.ExitUsage || code == domain.ExitPermissionDenied || code == domain.ExitBasisRequired)
		if !isRej && !alert {
			return Result{}, err
		}
		if rerr := x.st.Update(ctx, func(tx *store.Tx) error { return x.recordRejection(tx, spec, tr, rj, orig, alert) }); rerr != nil {
			return Result{}, fmt.Errorf("command: record rejection: %w (original: %v)", rerr, orig)
		}
		return Result{}, orig
	}
	if !res.Replayed {
		x.mu.RLock()
		subs := append([]func(Commit){}, x.subs...)
		x.mu.RUnlock()
		for _, f := range subs {
			f(commit)
		}
	}
	return res, nil
}

// recordRejection 在同一事务里写 command.rejected、跑拒绝回调、记回执与捕获的 health.alert（§14.5）。
func (x *Executor) recordRejection(tx *store.Tx, spec Spec, tr trace, rj *rejected, orig error, alert bool) error {
	why := orig.Error()
	var de *domain.Error
	if errors.As(orig, &de) {
		why = de.Why
	}
	code, at := exitOf(orig), x.now()
	if rj != nil {
		if _, err := eventlog.Append(tx, rj.ev); err != nil {
			return err
		}
		x.mu.RLock()
		hooks := append([]func(*store.Tx, Rejection) error{}, x.onRej...)
		x.mu.RUnlock()
		for _, f := range hooks {
			if err := f(tx, Rejection{Command: spec.Name, Actor: rj.ev.Actor, Object: tr.obj, Exit: code, Why: why, At: at}); err != nil {
				return err
			}
		}
	}
	if !alert {
		return nil
	}
	obj := tr.obj
	if obj == "" {
		obj = spec.Name
	}
	_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: obj, Actor: tr.actor, At: at,
		Reason:  spec.Name + " rejected: " + why,
		Payload: mustJSON(map[string]any{"kind": ReceiptKind, "command": spec.Name, "exit": code, "why": why})})
	return err
}

func (x *Executor) pipeline(tx *store.Tx, spec Spec, req Request, tr *trace) (Result, Commit, error) {
	// 1 认证
	caller, err := req.Identity.Authenticate(tx)
	if err != nil {
		return Result{}, Commit{}, err
	}
	tr.actor = caller.Actor
	if (caller.Kind == auth.Ext || caller.Kind == auth.Webhook) && !kindAllowed(spec, caller.Kind) {
		return Result{}, Commit{}, domain.NewError(domain.CodePermissionDenied, "credential kind cannot call this command", "use an owner or agent credential")
	}
	// 2 授权
	if err := checkInput(req.Input); err != nil {
		return Result{}, Commit{}, err
	}
	obj, err := spec.Object(tx, req.Input)
	if err != nil {
		return Result{}, Commit{}, err
	}
	tr.obj = obj.ID
	if caller.Level == authz.LevelRun && caller.RunScope != nil {
		if obj.InRunScope, obj.InRunProject, err = caller.RunScope(tx, obj); err != nil {
			return Result{}, Commit{}, err
		}
	}
	pv, pol, err := policy.Current(tx)
	if err != nil {
		return Result{}, Commit{}, err
	}
	actions := []authz.Action{spec.Action}
	if spec.Actions != nil {
		actions = spec.Actions(tx, caller, obj, req.Input)
	}
	var channel, sender string
	webURL := req.WebURL
	if spec.ChannelAnswer != nil {
		var w string
		if channel, sender, w, err = spec.ChannelAnswer(tx, caller, req.Input); err != nil {
			return Result{}, Commit{}, err
		}
		if w != "" {
			webURL = w
		}
	}
	tier := spec.Tier
	if spec.Bound != nil && caller.Level == authz.LevelDelegate {
		if bound, err := spec.Bound(tx, caller, obj, req.Input); err != nil {
			return Result{}, Commit{}, err
		} else if bound {
			tier = authz.TierWork
		}
	}
	var grant *policy.Grant
	if req.Grant != "" && caller.Level == authz.LevelDelegate {
		g, err := policy.GetGrant(tx, req.Grant)
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.CodeNotFound {
			err = nil // 授权判定按"授权单不存在"返回退出码 23
		} else if err == nil {
			grant = &g
		}
		if err != nil {
			return Result{}, Commit{}, err
		}
	}
	decided := authz.TierNone
	for _, act := range actions {
		ar := authz.Request{Actor: caller.Actor, Level: caller.Level, Action: act, Object: obj,
			Evidence: req.Evidence, Tier: tier, Owner: req.Owner, WebURL: webURL, Channel: channel, Sender: sender,
			Command: spec.Name, GrantID: req.Grant, Grant: grant, Credential: caller.Credential, Now: x.now()}
		d := authz.Decide(pol, ar)
		if d.Err == nil {
			decided = max(decided, authz.TierOf(ar))
			continue
		}
		if ev, ok := authz.RejectionEvent(ar, d); ok {
			ev.At = x.now()
			if ev.Object == "" { // 新建类命令没有对象：拒绝事件记在命令名下
				ev.Object = spec.Name
			}
			if caller.Actor.Via == domain.ViaRule {
				ev.Basis = &domain.Basis{PolicyVersion: pv, RuleID: req.RuleID}
			}
			return Result{}, Commit{}, &rejected{err: d.Err, ev: ev}
		}
		return Result{}, Commit{}, d.Err
	}
	// 第一档的 Event 不带 on_behalf_of（§14.5）：Bearer 认证只知道凭据，替本人说话要由第二档的依据来换。
	if caller.Level == authz.LevelDelegate && caller.Actor.Type == domain.ActorAgent && decided < authz.TierDecide {
		caller.Actor.OnBehalfOf = ""
	}
	usedGrant := ""
	if req.Grant != "" && caller.Level == authz.LevelDelegate && decided >= authz.TierDecide {
		usedGrant = req.Grant
	}
	// 3 校验：幂等 → 乐观版本 → 命令自身约束
	hash := sha256.Sum256(req.Input)
	inHash := hex.EncodeToString(hash[:])
	if req.IdemKey != "" {
		if prev, ok, err := loadIdem(tx, caller.Actor.Principal, spec.Name, req.IdemKey); err != nil {
			return Result{}, Commit{}, err
		} else if ok {
			if prev.inputHash != inHash {
				return Result{}, Commit{}, domain.NewError(domain.CodeConflict, "idempotency key was used with different input", "use a new key for a different request")
			}
			return Result{Value: json.RawMessage(prev.result), Version: prev.version, Replayed: true}, Commit{}, nil
		}
	}
	var cur int64
	if obj.ID != "" {
		if cur, err = loadVersion(tx, obj.ID); err != nil {
			return Result{}, Commit{}, err
		}
	}
	if spec.RequireVersion && req.ExpectVersion == nil {
		return Result{}, Commit{}, invalid("expected version is required")
	}
	if req.ExpectVersion != nil {
		if obj.ID == "" {
			return Result{}, Commit{}, invalid("expected version needs an object")
		}
		if *req.ExpectVersion != cur {
			return Result{}, Commit{}, domain.NewError(domain.CodeConflict, fmt.Sprintf("object is at version %d, not %d", cur, *req.ExpectVersion), "re-read the object and retry")
		}
	}
	if spec.Validate != nil {
		if err := spec.Validate(tx, obj, req.Input); err != nil {
			return Result{}, Commit{}, err
		}
	}
	// 规则动作必须继承（§14.9）；只有本人显式发起才开新链路。
	if caller.Actor.Via == domain.ViaRule && req.Chain.Mode == ChainAuto {
		return Result{}, Commit{}, invalid("rule action must inherit chain depth from a source object")
	}
	depth, err := resolveDepth(tx, spec, caller, obj, req.Chain)
	if err != nil {
		return Result{}, Commit{}, err
	}
	var basis *domain.Basis
	if caller.Actor.Via == domain.ViaRule {
		if req.RuleID == "" {
			return Result{}, Commit{}, invalid("rule action requires a rule id")
		}
	}
	if req.RuleID != "" || caller.Actor.Via == domain.ViaRule {
		basis = &domain.Basis{PolicyVersion: pv, RuleID: req.RuleID}
	}
	at := x.now()
	c := &Ctx{Tx: tx, Caller: caller, Input: req.Input, Object: obj, ChainDepth: depth, Basis: basis, Evidence: req.Evidence, Grant: usedGrant,
		now: func() time.Time { return at }}
	// 4 事务
	var out Result
	var value any
	// 超阈值只记录不派发（健康类投递由 kernel/health 据该事件统一产出）。本人在 Web 的操作照常生效：深度照样沿用，
	// 但本人就是熔断后的处理者，拦下他会让被熔断的事项既改不了也放弃不了。
	if depth > x.MaxChainDepth && caller.Level != authz.LevelWeb {
		if _, err := c.Emit(domain.Event{Type: domain.EvHealthAlert, Object: objectOrCommand(obj, spec),
			Reason:  "chain depth exceeded",
			Payload: mustJSON(map[string]any{"command": spec.Name, "chain_depth": depth, "threshold": x.MaxChainDepth})}); err != nil {
			return Result{}, Commit{}, err
		}
		out.Suppressed = true
		out.Version = cur
	} else {
		if value, err = spec.Apply(c); err != nil {
			return Result{}, Commit{}, err
		}
		out.Version = cur
		if obj.ID != "" {
			out.Version = cur + 1
			if _, err := tx.Exec(`INSERT INTO command_version (object, version) VALUES (?, ?)
				ON CONFLICT(object) DO UPDATE SET version = excluded.version`, obj.ID, out.Version); err != nil {
				return Result{}, Commit{}, err
			}
		}
		x.mu.RLock()
		hooks := append([]func(*Ctx) error{}, x.inTx...)
		x.mu.RUnlock()
		for _, f := range hooks {
			if err := f(c); err != nil {
				return Result{}, Commit{}, err
			}
		}
		if len(hooks) > 0 && obj.ID != "" { // 回调可能又改了对象（Bump），返回给调用方的版本取最新
			if out.Version, err = loadVersion(tx, obj.ID); err != nil {
				return Result{}, Commit{}, err
			}
		}
	}
	if out.Value, err = json.Marshal(value); err != nil {
		return Result{}, Commit{}, err
	}
	out.Seqs = c.seqs
	if req.IdemKey != "" {
		if _, err := tx.Exec(`INSERT INTO command_idem (principal, command, key, input_hash, result, version, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			caller.Actor.Principal, spec.Name, req.IdemKey, inHash, string(out.Value), out.Version, x.now().UnixNano()); err != nil {
			return Result{}, Commit{}, err
		}
	}
	if spec.Receipt { // 每次成功都留一个"最近成功"标记，供回执与捕获告警清除（不依赖调用方带幂等键）
		if _, err := tx.Exec(`INSERT INTO command_idem (principal, command, key, input_hash, result, version, created_at) VALUES (?, ?, ?, '', '', 0, ?)
			ON CONFLICT(principal, command, key) DO UPDATE SET created_at = excluded.created_at`,
			caller.Actor.Principal, spec.Name, lastOKKey, x.now().UnixNano()); err != nil {
			return Result{}, Commit{}, err
		}
	}
	return out, Commit{Command: spec.Name, Actor: caller.Actor, Object: obj.ID, Seqs: out.Seqs, Input: req.Input, Value: out.Value}, nil
}

// lastOKKey 是 Receipt 命令"最近成功"标记的键；含 NUL，HTTP 头与 CLI 参数都带不出来，不会与调用方的幂等键冲突。
const lastOKKey = "\x00last-ok"

func objectOrCommand(o authz.Object, s Spec) string {
	if o.ID != "" {
		return o.ID
	}
	return s.Name
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // 只用于固定结构，不会失败
	}
	return b
}

func kindAllowed(s Spec, k auth.Kind) bool {
	for _, a := range s.Kinds {
		if a == k {
			return true
		}
	}
	return false
}

type idemRow struct {
	inputHash, result string
	version           int64
}

func loadIdem(tx *store.Tx, principal, command, key string) (idemRow, bool, error) {
	r, err := tx.Query("SELECT input_hash, result, version FROM command_idem WHERE principal = ? AND command = ? AND key = ?", principal, command, key)
	if err != nil {
		return idemRow{}, false, err
	}
	defer r.Close()
	if !r.Next() {
		return idemRow{}, false, r.Err()
	}
	var row idemRow
	err = r.Scan(&row.inputHash, &row.result, &row.version)
	return row, err == nil, err
}

// SucceededSince 报告 principal 是否在 at 及之后成功执行过 command（Receipt 命令每次成功都更新一条标记，与调用方是否带幂等键无关；复用 command_idem，不新增存储）。
// 回执与捕获被拒绝的健康告警据此清除（§15.2）。
func SucceededSince(tx *store.Tx, principal, command string, at time.Time) (bool, error) {
	r, err := tx.Query("SELECT 1 FROM command_idem WHERE principal = ? AND command = ? AND created_at >= ? LIMIT 1", principal, command, at.UnixNano())
	if err != nil {
		return false, err
	}
	defer r.Close()
	ok := r.Next()
	return ok, r.Err()
}

// Version 返回对象当前版本，没有写入过为 0。
func Version(tx *store.Tx, object string) (int64, error) { return loadVersion(tx, object) }

// Bump 把对象版本加一并返回新版本。命令管线之外在自己事务里改对象的内部入口（如时钟处理器）用它，
// 使乐观版本仍只有一个来源；命令内部不要调用，管线已经加过。
func Bump(tx *store.Tx, object string) (int64, error) {
	cur, err := loadVersion(tx, object)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO command_version (object, version) VALUES (?, ?)
		ON CONFLICT(object) DO UPDATE SET version = excluded.version`, object, cur+1); err != nil {
		return 0, err
	}
	return cur + 1, nil
}

func loadVersion(tx *store.Tx, object string) (int64, error) {
	r, err := tx.Query("SELECT version FROM command_version WHERE object = ?", object)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	var v int64
	if r.Next() {
		err = r.Scan(&v)
	} else {
		err = r.Err()
	}
	return v, err
}

// resolveDepth 得出本命令的链路深度（§14.9）。继承规则只在这里：
//   - 运行凭据发出的命令一律继承自本次运行并加一。基数取运行记录的深度（拉起时已按规则加过一），
//     不取目标事项最近一条事件，否则同一运行连写几次就把深度推高；
//   - 进程内调用方指明了来源（规则、时钟、挂靠消息）：按来源对象最近一条事件；
//   - 未指明（对外入口都如此）：本人在 Web 显式委派开始新链路；其余命令沿用对象当前深度，
//     回执、委派、提问与回答都不清零。新建的对象还没有事件，从 0 开始。
func resolveDepth(tx *store.Tx, spec Spec, caller Caller, obj authz.Object, c Chain) (int, error) {
	if caller.Level == authz.LevelRun {
		return caller.RunDepth + 1, nil
	}
	if c.Mode == ChainAuto {
		if obj.ID == "" || (spec.Action == authz.ActionDelegate && caller.Level == authz.LevelWeb) {
			return 0, nil
		}
		c = Chain{Mode: ChainKeep, From: obj.ID}
	}
	return chainDepth(tx, c)
}

// chainDepth 从账本读取来源对象最近一条事件的深度（shortcut: 读全部事件取末条，事件量大时给 eventlog 加按对象取末条的接口）。
func chainDepth(tx *store.Tx, c Chain) (int, error) {
	if c.Mode != ChainInherit && c.Mode != ChainKeep {
		return 0, invalid("unknown chain mode")
	}
	if c.From == "" {
		return 0, invalid("chain source object is required")
	}
	es, err := eventlog.ReadObject(tx, c.From)
	if err != nil {
		return 0, err
	}
	// 拒绝事件不带来源深度（记为 0），不能让一次被拒绝的尝试把链路清零。
	d := 0
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Event.Type != domain.EvCommandRejected {
			d = es[i].Event.ChainDepth
			break
		}
	}
	if c.Mode == ChainInherit {
		d++
	}
	return d, nil
}
