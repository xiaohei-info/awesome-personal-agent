package executors

// 选人与拉起（§13.6、§14.5、附录 A.2）："谁来干活"只有执行者一个概念。每个选人的位置存执行者 id；
// 执行者所属的声明（harness）决定怎么拉起、用哪张权限卡；执行者的 overrides 是默认值，handle 经 executor_args 成为执行者参数。
// 所有后台运行（委派即开工与 @、规则后台运行、任务型、来源批处理、续接）都经 Resolve 选出 Target、经 Target.Build 拼出命令，
// 不再各自拼 argv。旧的 Agent 卡（executors_agent）已取消（2026-11-28），只读保留给执行者统一切换（LegacyCards）。

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// LegacyCard 是已取消的 Agent 卡（executors_agent 的一行）：只读，供执行者统一切换与旧账本的权限卡换算读取。
type LegacyCard struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Harness string            `json:"harness"`
	Values  map[string]string `json:"values,omitempty"`
	Enabled bool              `json:"enabled"`
	Rev     int64             `json:"rev"`
}

// LegacyCards 读出全部旧 Agent 卡，按名字排序。
func LegacyCards(tx *store.Tx) ([]LegacyCard, error) {
	cards := []LegacyCard{}
	err := query(tx, func(rows *store.Rows) error {
		var c LegacyCard
		var options string
		if err := rows.Scan(&c.ID, &c.Harness, &c.Name, &options, &c.Enabled, &c.Rev); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(options), &c.Values); err != nil {
			return err
		}
		cards = append(cards, c)
		return nil
	}, `SELECT id,harness,name,options,enabled,rev FROM executors_agent ORDER BY name,id`)
	return cards, err
}

// 选择来源（写进运行快照的 selection_source，Web 按词表显示）。
const (
	SourceExplicit = "explicit" // 调用方直接给出（委派、@、规则动作里写明的 executor）
	SourceItem     = "item"     // 事项上指定的执行者
	SourceSchedule = "schedule" // 安排（实例参数 executor）
	SourceRule     = "rule"     // 规则固定用的执行者（rule_executor）
	SourceDefault  = "default"  // 分工：日常执行者
	SourceMain     = "main"     // 分工：主执行者
	SourcePipeline = "pipeline" // 来源处理线设置里选定的执行者
	SourceResume   = "resume"   // 续接原运行的执行者
)

// 运行者（与 domain.Run.Runner 一致）。
const (
	RunnerTask        = "task"
	RunnerMainSession = "main_session"
	RunnerSourceBatch = "source_batch"
)

// Selection 是一次拉起可用的选择：Item 是事项上指定的执行者，Schedule 是安排参数里的执行者，Rule 是规则键。
type Selection struct {
	Item, Schedule, Rule, Runner string
}

// Pick 按 事项 > 安排 > 规则 > 分工 选出执行者 id（§13.6）；分工在主会话取主执行者，其余取日常执行者。没选为空。
func Pick(pol policy.Policy, sel Selection) (id, source string) {
	switch {
	case sel.Item != "":
		return sel.Item, SourceItem
	case sel.Schedule != "":
		return sel.Schedule, SourceSchedule
	case pol.RuleExecutor[sel.Rule] != "" && sel.Rule != "":
		return pol.RuleExecutor[sel.Rule], SourceRule
	case sel.Runner == RunnerMainSession:
		return pol.MainExecutor, SourceMain
	}
	return pol.DefaultExecutor, SourceDefault
}

// Want 是要拉起谁：Executor 为空表示没选。Delegated 为真表示运行目标是委派子事项（即开工、@、runner=task 的规则）：
// 只有这种运行能交给不在 APA 主机上的执行者（有跨机模板与结构化回报，§13.6）。Host 是 APA 主机名。
type Want struct {
	Executor, Source, Runner, Host string
	Delegated                      bool
}

// Target 是选中的执行者及其声明；Reason 非空表示现在拉不起（给人看的原因，调用方按各自的口径交回、告警或拒绝）。
type Target struct {
	Entry  Entry
	Record Record
	Key    string // 所属声明，也是权限卡的键
	Access string // 权限卡上的工具档
	Remote bool
	Source string
	Runner string
	Reason string
}

// Name 是执行者给人看的名字。
func (t Target) Name() string {
	if t.Entry.Label != "" {
		return t.Entry.Label
	}
	return t.Entry.ID
}

// Program 是运行记录的 harness：隔离另写的声明记它所属的程序，其余就是声明本身。
func (t Target) Program() string {
	if t.Record.Declaration.Base != "" {
		return t.Record.Declaration.Base
	}
	return t.Key
}

// Remote 报告执行者是否不在 APA 主机 host 上：自报了主机且不是本机（短名也算本机）。这样的运行不发运行凭据（§13.6）。
func Remote(e Entry, host string) bool {
	return e.Host != "" && !strings.EqualFold(e.Host, host) && !strings.EqualFold(e.Host, strings.Split(host, ".")[0])
}

var sourceWord = map[string]string{SourceItem: "事项指定的", SourceSchedule: "安排指定的", SourceRule: "规则指定的",
	SourceDefault: "日常", SourceMain: "主", SourcePipeline: "来源处理的", SourceResume: "原运行的", SourceExplicit: ""}

// Resolve 取出执行者 w.Executor 与它的生效声明，判定现在能不能拉起：在名录里、由已确认的无头运行声明登记、
// 不在本机时只用于委派、声明提供权限卡上的那一档工具（§14.5）。以本人身份运行（main_session）不另要求声明：
// 声明的 main_session.args 有就追加。
func Resolve(tx *store.Tx, pol policy.Policy, w Want) (t Target, err error) {
	t.Source, t.Runner = w.Source, w.Runner
	who := sourceWord[w.Source] + "执行者"
	if w.Executor == "" {
		t.Reason = "还没选" + who
		return t, nil
	}
	e, ok, err := Get(tx, w.Executor)
	if err != nil {
		return t, err
	}
	if !ok {
		t.Reason = who + " " + w.Executor + " 不在执行者名录里"
		return t, nil
	}
	t.Entry, t.Key, t.Remote = e, e.Harness, Remote(e, w.Host)
	if e.Harness != "" {
		var found bool
		if t.Record, found, err = record(tx, e.Harness, 0, StateActive); err != nil {
			return t, err
		}
		if !found {
			t.Record = Record{}
		}
	}
	t.Access = pol.Card(t.Key).Tool
	switch d := t.Record.Declaration; {
	case e.Harness == "" || d.Headless == nil:
		t.Reason = who + " " + t.Name() + " 没有已确认的无头运行声明，APA 不知道怎么拉起它"
	case t.Remote && !w.Delegated:
		t.Reason = who + " " + t.Name() + " 不在 APA 主机上（" + e.Host + "），读不到账本，只能经委派派活"
	default:
		t.Reason = t.AccessReason(t.Access)
	}
	return t, nil
}

// AccessReason 判定声明是否提供 request 这一档、权限卡是否放到这一档；都满足为空。
func (t Target) AccessReason(request string) string {
	if t.Reason != "" {
		return t.Reason
	}
	ranks := map[string]int{"read": 1, "edit": 2, "full": 3}
	if _, ok := t.Record.Declaration.Headless.Access[request]; !ok {
		return "无头运行声明未提供权限档位：" + request
	}
	if ranks[request] == 0 || ranks[request] > ranks[t.Access] {
		return fmt.Sprintf("权限卡上的工具档位已调低：这次运行要 %s，%s 现在是 %s", request, t.Name(), t.Access)
	}
	return ""
}

// Spec 是一次拉起的运行参数：Once 是单次覆盖（只能取声明列出的值）；Resume 非空时按声明的 resume 接着这个会话，
// ContextName 是续接沿用的运行上下文文件名（空为运行 id）。ContextDir 为空或执行者不在本机时 {run_context} 展开为空。
type Spec struct {
	RunID, RunDir, Prompt, ContextDir, ContextName, Resume string
	Once                                                   map[string]string
}

// Built 是拼好的命令与运行记录要写的字段。
type Built struct {
	Command, Cancel, Await []string
	SessionID              string
	Resumed                bool
	Effective              map[string]string
	Snapshot               domain.AgentSnapshot
}

// ReportSchemaFile 是内核写进运行目录的回报结构文件名（{report_schema}）。
const ReportSchemaFile = "report-schema.json"

// Build 是所有后台运行唯一的拼装（附录 A.2）：命令模板（展开占位符，执行者参数与覆盖放在 {executor_args} 处，没有就追加）
// → 人格（本人没开"带人格和记忆"时 no_persona）→ 思考档（设置的"后台 Agent 运行"；执行者有生效的思考档覆盖时不再追加）
// → output → 主会话参数 → 工具档 → 跨机参数 → 续接或指定会话；另给出取消、续等命令与运行快照。调用方先确认 Reason 为空。
func (t Target) Build(pol policy.Policy, s Spec) (Built, error) {
	d := t.Record.Declaration
	hl := *d.Headless
	l, err := d.LaunchFor(t.Entry, s.Once)
	if err != nil {
		return Built{}, err
	}
	resumed := s.Resume != "" && len(hl.Resume) > 0
	runContext := ""
	if !t.Remote && s.ContextDir != "" {
		name := s.RunID
		if resumed && s.ContextName != "" { // 续接同一会话沿用原来的运行上下文文件（proc.Spec.ContextName）
			name = s.ContextName
		}
		runContext = filepath.Join(s.ContextDir, name)
	}
	schema := ""
	if s.RunDir != "" {
		schema = filepath.Join(s.RunDir, ReportSchemaFile)
	}
	vals := map[string]string{"{prompt}": s.Prompt, "{run_dir}": s.RunDir, "{run_context}": runContext, "{run_id}": s.RunID, "{report_schema}": schema}
	argv := Expand(hl.Command, vals, l.Args)
	if !pol.BackgroundPersona {
		argv = append(argv, hl.NoPersona...)
	}
	if bg := pol.Thinking.Effective().Background; bg != policy.ThinkingOff && l.Thinking == "" {
		for _, a := range hl.Effort {
			argv = append(argv, strings.ReplaceAll(a, "{effort}", bg))
		}
	}
	argv = append(argv, hl.Output...)
	if t.Runner == RunnerMainSession && d.MainSession != nil {
		argv = append(argv, d.MainSession.Args...)
	}
	argv = append(argv, hl.Access[t.Access]...)
	if t.Remote {
		argv = append(argv, Expand(hl.RemoteArgs, vals, nil)...)
	}
	b := Built{Effective: l.Effective}
	switch {
	case resumed:
		b.SessionID, b.Resumed = s.Resume, true
		argv = append(argv, replaceAll(hl.Resume, "{session_id}", s.Resume)...)
	case len(hl.Session.Assign) > 0:
		b.SessionID = s.RunID
		argv = append(argv, replaceAll(hl.Session.Assign, "{session_id}", s.RunID)...)
	}
	b.Command = argv
	b.Cancel = hl.CancelArgv(d, t.Entry, s.RunID, b.SessionID)
	b.Await = hl.AwaitArgv(d, t.Entry, s.RunID, b.SessionID)
	b.Snapshot = domain.AgentSnapshot{AgentID: t.Key, AgentName: t.Name(), Executor: t.Entry.ID, DeclarationRev: t.Record.Rev,
		SelectionSource: t.Source, PermissionCap: t.Access, ConfiguredModel: l.Model, ConfiguredThinking: l.Thinking}
	return b, nil
}

func replaceAll(in []string, old, v string) []string {
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = strings.ReplaceAll(a, old, v)
	}
	return out
}

// CheckRun 在入队与出队时按运行快照复核（§14.5）：所属声明仍是拉起时那一版，权限卡仍放到这次的工具档。
// 快照里没有执行者的旧运行（执行者统一切换前入队）按声明复核。
func CheckRun(tx *store.Tx, run domain.Run) error {
	snap := run.AgentSnapshot
	if snap == nil {
		return nil
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	key := policy.AgentKey(snap.AgentID)
	t := Target{Key: key, Access: pol.Card(key).Tool, Entry: Entry{Executor: Executor{ID: snap.Executor, Label: snap.AgentName}}}
	rec, ok, err := record(tx, key, 0, StateActive)
	if err != nil {
		return err
	}
	if !ok || rec.Declaration.Headless == nil {
		return conflict("%s 没有已确认的无头运行声明", t.Name())
	}
	t.Record = rec
	if snap.Executor != "" {
		if e, ok, err := Get(tx, snap.Executor); err != nil {
			return err
		} else if !ok || e.Harness != key {
			return conflict("执行者 %s 已不在名录里或换了声明，请重新运行", snap.Executor)
		}
	}
	if reason := t.AccessReason(run.Access); reason != "" {
		return conflict("%s", reason)
	}
	if rec.Rev != snap.DeclarationRev {
		return conflict("启动声明已变更，请重新运行")
	}
	return nil
}

// 主会话不能唤醒的原因（MainWake 的 reason）：稳定类别码，Web 按词表说明。
const (
	MainNotSet      = "main_not_set"     // 没选主执行者
	MainUnavailable = "main_unavailable" // 选了，但不在名录、没有已确认的声明或不支持主会话
	MainCannotWake  = "main_cannot_wake" // 主执行者的声明不支持唤醒（网关插件没启用等）
)

// MainWake 找主执行者的主会话（§13.6）：能唤醒时返回唤醒路由用的 Agent（与唤醒运行同一口径：隔离另写的声明为声明键，
// 其余为空），否则 reason 给出原因。只读。
func MainWake(tx *store.Tx, pol policy.Policy) (agent, reason string, err error) {
	if pol.MainExecutor == "" {
		return "", MainNotSet, nil
	}
	// 唤醒不在本机起进程，不看工具档与执行者在哪台机器：只看执行者在、声明已确认且声明支持主会话。
	e, ok, err := Get(tx, pol.MainExecutor)
	if err != nil || !ok {
		return "", MainUnavailable, err
	}
	rec, ok, err := record(tx, e.Harness, 0, StateActive)
	switch d := rec.Declaration; {
	case err != nil:
		return "", "", err
	case !ok || d.Headless == nil:
		return "", MainUnavailable, nil
	case !d.MainSession.CanWake():
		return "", MainCannotWake, nil
	case d.Base == "":
		return "", "", nil
	}
	return e.Harness, "", nil
}

// MainAgent 是主执行者所属的声明（委派结果"发起方是不是主 Agent"按它比，§13.6）；没选或不在名录为空。
func MainAgent(tx *store.Tx, pol policy.Policy) (string, error) {
	if pol.MainExecutor == "" {
		return "", nil
	}
	e, _, err := Get(tx, pol.MainExecutor)
	return e.Harness, err
}

// Pipeline 是跑来源批处理的执行者：来源处理线设置里选定的，没选时取日常执行者，不取主执行者（§9.3）。
func Pipeline(pol policy.Policy) (id, source string) {
	if pol.SourcePipeline.Executor != "" {
		return pol.SourcePipeline.Executor, SourcePipeline
	}
	return pol.DefaultExecutor, SourceDefault
}

// ForDeclaration 是执行者统一切换前入队的运行（快照里只有 Agent 卡 id）对应的执行者（§16.5）：声明下只有一个执行者时取它，
// 否则取 id 与声明同名的那个（兼容卡迁来的）；都没有为空。
func ForDeclaration(tx *store.Tx, agentID string) (string, error) {
	key := policy.AgentKey(agentID)
	es, err := List(tx)
	if err != nil {
		return "", err
	}
	var under []string
	for _, e := range es {
		if e.Harness == key {
			under = append(under, e.ID)
		}
	}
	if len(under) == 1 {
		return under[0], nil
	}
	for _, id := range under {
		if id == key {
			return id, nil
		}
	}
	return "", nil
}
