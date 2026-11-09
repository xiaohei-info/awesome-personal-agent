package agentsmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 三态（附录 B.2）与对应的 CLI 退出码（§12.5）。
const (
	StateCurrent = "current"
	StateStale   = "stale"
	StateDrift   = "drift"

	ExitStale = domain.ExitAgentsStale
	ExitDrift = domain.ExitAgentsDrift
)

// 漂移事项的三个选项，以及漂移登记的状态：open 等本人选，其余是选择的结果。
const (
	ChoiceMerge    = "merge"
	ChoiceOverride = "override"
	ChoiceIgnore   = "ignore"

	driftOpen     = "open"
	driftMerged   = "merge"
	driftOverride = "override"
	driftIgnored  = "ignore"
)

// 重新渲染的时机：规范生效、托管文档登记或路径变化、仓库增删。
var rerenderOn = []string{"revision.confirm", "material.add", "material.refresh", "material.relocate", "project.repo.add", "project.repo.remove"}

// Options 注入本模块读取但不拥有的东西。
type Options struct {
	// Template 是 agents-md 包的 templates/agents.md，必须含五个占位符；能力包热加载后用 SetTemplate 换新。
	Template string
	// Fetch 读仓库默认分支上的 AGENTS.md（经外呼调用器拉起的只读读取器）；仅 Trigger 用，可空。
	Fetch func(ctx context.Context, repo string) (content string, exists bool, err error)
	// OnError 接收提交后自动重新渲染失败的错误，必填：失败时渲染记录会停在旧值，不能静默。
	OnError func(err error)
}

// Service 提供 AGENTS.md 渲染与漂移判定。
type Service struct {
	x   *command.Executor
	opt Options
	tpl atomic.Pointer[string]
	// merging 是已在事务里选了"合回"、等提交后补提 proposed 修订的漂移事项。
	mu      sync.Mutex
	merging []string
}

func checkTemplate(tpl string) error {
	for _, ph := range placeholders {
		if strings.Count(tpl, "{"+ph+"}") != 1 {
			return fmt.Errorf("agentsmd: template must use {%s} exactly once", ph)
		}
	}
	return nil
}

// SetTemplate 换用新的渲染模板（能力包热加载）；不合格时拒绝并保留当前模板。已有渲染记录不动，下一次渲染才按新模板。
func (s *Service) SetTemplate(tpl string) error {
	if err := checkTemplate(tpl); err != nil {
		return err
	}
	s.tpl.Store(&tpl)
	return nil
}

var system = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "kernel:agentsmd", Via: domain.ViaSource}, Level: authz.LevelWorker}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func conflict(why, next string) error { return domain.NewError(domain.CodeConflict, why, next) }

func notFound(what, id string) error {
	return domain.NewError(domain.CodeNotFound, what+" "+id+" does not exist", "register the repository on a project and retry")
}

func decode(in json.RawMessage, v any) error {
	if len(bytes.TrimSpace(in)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

// New 登记 agents.render、agents.check、agents.sync、agents.resolve，并订阅会改变渲染输入的命令。
//
// 授权（§14.3）：render 按"修订材料"（worker 以上）；check 与 sync 是 observer 即可的读取加记录；
// resolve 按代答（Web 本人可；delegate 必须附本人原话；observer、worker、运行凭据得到 4）。
func New(x *command.Executor, o Options) (*Service, error) {
	if err := checkTemplate(o.Template); err != nil {
		return nil, err
	}
	if o.OnError == nil {
		return nil, errors.New("agentsmd: Options.OnError is required")
	}
	s := &Service{x: x, opt: o}
	s.tpl.Store(&o.Template)
	for _, sp := range []command.Spec{s.renderCmd(), s.checkCmd("agents.check", false), s.checkCmd("agents.sync", true), s.resolveCmd()} {
		if err := x.Register(sp); err != nil {
			return nil, err
		}
	}
	x.InTx(s.onDecision)
	x.OnCommit(func(command.Commit) { s.flushMerges() })
	x.OnCommit(func(c command.Commit) {
		if !slices.Contains(rerenderOn, c.Command) {
			return
		}
		if _, err := x.Do(context.Background(), command.Request{Identity: system, Command: "agents.render", Input: json.RawMessage(`{}`)}); err != nil {
			o.OnError(err)
		}
	})
	return s, nil
}

// ---- agents.render ----

// RenderOut 列出每个仓库本次渲染的结果；Changed 为真表示产生了新的渲染记录。
type RenderOut struct {
	Renders []RenderResult `json:"renders"`
}

// RenderResult 是一个仓库的渲染结果。
type RenderResult struct {
	Repo    string `json:"repo"`
	Hash    string `json:"hash"`
	Changed bool   `json:"changed"`
}

func (s *Service) renderCmd() command.Spec {
	return command.Spec{Name: "agents.render", Action: authz.ActionReviseMaterial,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v struct{}
			return authz.Object{}, decode(in, &v)
		},
		Apply: func(c *command.Ctx) (any, error) {
			ps, err := projects.List(c.Tx)
			if err != nil {
				return nil, err
			}
			out := RenderOut{Renders: []RenderResult{}}
			for _, p := range ps {
				for _, r := range p.Repos {
					rec, changed, err := renderRepo(c, *s.tpl.Load(), p.ID, r.Remote)
					if err != nil {
						return nil, err
					}
					out.Renders = append(out.Renders, RenderResult{Repo: r.Remote, Hash: rec.Hash, Changed: changed})
				}
			}
			return out, nil
		}}
}

// ---- agents.check / agents.sync ----

type checkIn struct {
	Repo string `json:"repo"`
	Host string `json:"host"`
	// 工作区文件的上报方式三选一：content（完整内容，漂移时附 diff 必须用它）、file_hash（只报 hash）、missing（文件不存在）。
	Content  *string `json:"content"`
	FileHash string  `json:"file_hash"`
	Missing  bool    `json:"missing"`
}

// Verdict 是一次检查的结论。Exit 是 CLI 退出码：一致 0、过期 10、漂移 11。
type Verdict struct {
	State      string `json:"state"`
	Exit       int    `json:"exit"`
	FileHash   string `json:"file_hash,omitempty"`
	RenderHash string `json:"render_hash"`
	// Prompt：过期且这个渲染 hash 还没提示过；同一仓库同一渲染 hash 只提示一次。
	Prompt bool `json:"prompt,omitempty"`
	// Item 是漂移对应的 needs_input 事项；Decision 是本人对它的选择（open 表示还没选）。
	Item     string `json:"item,omitempty"`
	Decision string `json:"decision,omitempty"`
}

// SyncOut 是 agents.sync 的结果：内核不写文件，只说该不该写、写什么。Blocked 时 CLI 退出码为 6，手改内容原样保留。
type SyncOut struct {
	Verdict
	Write   bool   `json:"write"`
	Content string `json:"content,omitempty"`
	Blocked bool   `json:"blocked,omitempty"`
}

func (in checkIn) validate() error {
	n := 0
	if in.Content != nil {
		n++
	}
	if in.FileHash != "" {
		n++
	}
	if in.Missing {
		n++
	}
	switch {
	case strings.TrimSpace(in.Repo) == "":
		return invalid("repo is required")
	case strings.TrimSpace(in.Host) == "":
		return invalid("host is required")
	case n != 1:
		return invalid("give exactly one of content, file_hash or missing")
	}
	return nil
}

func (s *Service) checkCmd(name string, sync bool) command.Spec {
	parse := func(in json.RawMessage) (checkIn, error) {
		var v checkIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: name, Action: authz.ActionRead,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			if strings.TrimSpace(v.Repo) == "" {
				return authz.Object{}, invalid("repo is required")
			}
			p, ok, err := projects.ByRemote(tx, v.Repo)
			if err == nil && !ok {
				err = notFound("repository", v.Repo)
			}
			return authz.Object{Project: p.ID}, err
		},
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			return v.validate()
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			vd, rec, state, err := s.evaluate(c, v)
			if err != nil || !sync {
				return vd, err
			}
			out := SyncOut{Verdict: vd}
			switch {
			case vd.State == StateStale, vd.State == StateDrift && state == driftOverride:
				out.Write, out.Content = true, rec.Content
			case vd.State == StateDrift:
				// 手改内容不丢：未选"覆盖"之前不写。用结果而不是错误返回，保证 check 阶段生成的漂移事项照样落账。
				out.Blocked, out.Exit = true, domain.ExitConflict
			}
			return out, nil
		}}
}

// evaluate 是 check 与 sync 共用的判定：确保渲染记录最新，比对文件，登记最近一次检查，漂移时生成事项。
func (s *Service) evaluate(c *command.Ctx, in checkIn) (Verdict, Render, string, error) {
	p, reg, ok, err := projects.RepoOf(c.Tx, in.Repo)
	if err != nil {
		return Verdict{}, Render{}, "", err
	}
	if !ok {
		return Verdict{}, Render{}, "", notFound("repository", in.Repo)
	}
	in.Repo = reg.Remote // 渲染、检查、漂移三张表都按登记写法记账，调用方用哪种写法都落到同一行
	rec, _, err := renderRepo(c, *s.tpl.Load(), p.ID, in.Repo)
	if err != nil {
		return Verdict{}, rec, "", err
	}
	fh := in.FileHash
	if in.Content != nil {
		fh = FileHash(*in.Content)
	}
	vd := Verdict{FileHash: fh, RenderHash: rec.Hash}
	known := false
	if fh != "" {
		_, known, err = renderByHash(c.Tx, in.Repo, fh)
		if err != nil {
			return vd, rec, "", err
		}
	}
	state := ""
	switch {
	case fh == rec.Hash:
		vd.State = StateCurrent
	case in.Missing || known:
		vd.State, vd.Exit = StateStale, ExitStale
	case in.Content == nil:
		return vd, rec, "", invalid("the file hash matches no rendering: resend with content so the diff can be attached")
	default:
		vd.State, vd.Exit = StateDrift, ExitDrift
		if vd.Item, state, err = s.raiseDrift(c, p, rec, fh, *in.Content); err != nil {
			return vd, rec, "", err
		}
		vd.Decision = state
	}
	if err := s.record(c, in, &vd); err != nil {
		return vd, rec, "", err
	}
	return vd, rec, state, nil
}

// record 登记最近一次检查（GET /agents-md 的 last_check），并决定过期是否要提示。
func (s *Service) record(c *command.Ctx, in checkIn, vd *Verdict) error {
	var prompted string
	r, err := c.Tx.Query("SELECT prompted_hash FROM agentsmd_check WHERE repo = ?", in.Repo)
	if err != nil {
		return err
	}
	if r.Next() {
		err = r.Scan(&prompted)
	}
	r.Close()
	if err != nil {
		return err
	}
	if vd.State == StateStale && prompted != vd.RenderHash {
		vd.Prompt, prompted = true, vd.RenderHash
	}
	_, err = c.Tx.Exec(`INSERT INTO agentsmd_check (repo, state, file_hash, render_hash, host, checked_at, prompted_hash) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo) DO UPDATE SET state = excluded.state, file_hash = excluded.file_hash, render_hash = excluded.render_hash,
		host = excluded.host, checked_at = excluded.checked_at, prompted_hash = excluded.prompted_hash`,
		in.Repo, vd.State, vd.FileHash, vd.RenderHash, in.Host, c.Now().UnixNano(), prompted)
	return err
}

type drift struct{ item, state string }

func driftOf(tx *store.Tx, repo, fileHash, renderHash string) (d drift, ok bool, err error) {
	r, err := tx.Query("SELECT item, state FROM agentsmd_drift WHERE repo = ? AND file_hash = ? AND render_hash = ?", repo, fileHash, renderHash)
	if err != nil {
		return d, false, err
	}
	defer r.Close()
	if !r.Next() {
		return d, false, r.Err()
	}
	return d, true, r.Scan(&d.item, &d.state)
}

// raiseDrift 为一次漂移生成 needs_input 事项（附 diff，选项合回、覆盖、本次忽略）。
// 同一 (仓库, 文件, 渲染) 已有登记时沿用，不重复打扰；本人已选"本次忽略"也沿用，渲染变了才重新提。
func (s *Service) raiseDrift(c *command.Ctx, p domain.Project, rec Render, fileHash, content string) (item, state string, err error) {
	d, ok, err := driftOf(c.Tx, rec.Repo, fileHash, rec.Hash)
	if err != nil {
		return "", "", err
	}
	if ok {
		if d.state == driftOpen {
			// 事项被决策回答以外的途径关掉了（回答本身由 onDecision 落实所选项）：按"本次忽略"处理，什么也不写、不丢手改内容。
			it, err := items.Get(c.Tx, d.item)
			if err != nil {
				return "", "", err
			}
			if it.Phase != domain.PhaseNeedsInput {
				d.state = driftIgnored
				_, err = c.Tx.Exec("UPDATE agentsmd_drift SET state = ? WHERE item = ?", d.state, d.item)
			}
			return d.item, d.state, err
		}
		return d.item, d.state, nil
	}
	opts := []domain.AskOption{
		{ID: ChoiceMerge, Label: "合回：把手改内容提交为项目规范的修订，确认后重新渲染", Next: domain.PhaseDone},
		{ID: ChoiceOverride, Label: "覆盖：用渲染结果覆盖手改内容（之后 apa agents sync 才写入）", Next: domain.PhaseDone},
		{ID: ChoiceIgnore, Label: "本次忽略：不写文件", Next: domain.PhaseDone},
	}
	child := domain.Item{Phase: domain.PhaseNeedsInput, Kind: domain.KindTask, Priority: domain.PriorityNormal,
		Title:   "AGENTS.md 被手改：" + rec.Repo,
		Summary: "工作区的 AGENTS.md 与最新渲染不一致，也不是任何历史渲染",
		Body:    diffText(rec.body, FileBody(content)),
		Project: p.ID,
		Ask: &domain.Ask{AskInput: domain.AskInput{
			Question: "仓库 " + rec.Repo + " 的 AGENTS.md 被手改过（diff 见正文）。合回成项目规范的修订、用渲染结果覆盖，还是本次忽略？",
			Options:  opts, Recommend: ChoiceMerge,
		}},
		ItemKernelState: domain.ItemKernelState{ChainDepth: c.ChainDepth}}
	child, snap, err := items.Create(c.Tx, child, "", c.Now())
	if err != nil {
		return "", "", err
	}
	if _, err := command.Bump(c.Tx, child.ID); err != nil { // 新对象不是管线的授权对象，版本自己加
		return "", "", err
	}
	if _, err := c.Emit(domain.Event{Type: domain.EvItemCreated, Object: child.ID, Reason: "AGENTS.md drift", Payload: snap}); err != nil {
		return "", "", err
	}
	_, err = c.Tx.Exec("INSERT INTO agentsmd_drift (item, repo, file_hash, render_hash, content, state) VALUES (?, ?, ?, ?, ?, ?)",
		child.ID, rec.Repo, fileHash, rec.Hash, content, driftOpen)
	return child.ID, driftOpen, err
}

// ---- agents.resolve ----

// ResolveIn 是对漂移事项的选择。
type ResolveIn struct {
	Item   string `json:"item"`
	Choice string `json:"choice"`
	Reason string `json:"reason,omitempty"`
}

// ResolveOut：合回时 Revision 是生成的 proposed 修订（确认后才重新渲染）。
type ResolveOut struct {
	Item     string `json:"item"`
	Choice   string `json:"choice"`
	Material string `json:"material,omitempty"`
	Revision string `json:"revision,omitempty"`
}

func (s *Service) resolveCmd() command.Spec {
	parse := func(in json.RawMessage) (ResolveIn, error) {
		var v ResolveIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "agents.resolve", Action: authz.ActionAnswerDecision,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			it, err := items.Get(tx, v.Item)
			if err != nil {
				return authz.Object{}, err
			}
			o := authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Tags: it.Tags, Project: it.Project}
			if a := it.Ask; a != nil {
				o.Category, o.ScreeningPending = a.RiskClass, a.ScreeningPending
			}
			return o, nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			if !slices.Contains([]string{ChoiceMerge, ChoiceOverride, ChoiceIgnore}, v.Choice) {
				return invalid("choice must be merge, override or ignore")
			}
			d, err := driftByItem(tx, v.Item)
			if err != nil {
				return err
			}
			if d.state != driftOpen {
				return conflict("this drift was already decided: "+d.state, "the first choice stands; read the item")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			via := decisions.ViaChat
			if c.Caller.Level == authz.LevelWeb {
				via = decisions.ViaWeb
			}
			if err := decisions.Answer(c, v.Item, v.Choice, "", via, c.Caller.Actor.Principal, v.Reason); err != nil {
				return nil, err
			}
			_, err := c.Tx.Exec("UPDATE agentsmd_drift SET state = ? WHERE item = ?", v.Choice, v.Item)
			return ResolveOut{Item: v.Item, Choice: v.Choice}, err
		}}
}

type driftRow struct {
	drift
	repo, fileHash, renderHash, content string
}

func findDrift(tx *store.Tx, item string) (d driftRow, ok bool, err error) {
	r, err := tx.Query("SELECT item, state, repo, file_hash, render_hash, content FROM agentsmd_drift WHERE item = ?", item)
	if err != nil {
		return d, false, err
	}
	defer r.Close()
	if !r.Next() {
		return d, false, r.Err()
	}
	return d, true, r.Scan(&d.item, &d.state, &d.repo, &d.fileHash, &d.renderHash, &d.content)
}

func driftByItem(tx *store.Tx, item string) (driftRow, error) {
	d, ok, err := findDrift(tx, item)
	if err == nil && !ok {
		err = domain.NewError(domain.CodeNotFound, "item "+item+" is not an AGENTS.md drift", "pass the item from the check result")
	}
	return d, err
}

// mergePlan 是合回要提交的修订：内容，以及落在哪份项目规范上（material.ID 为空表示项目还没有规范，新建）。
type mergePlan struct {
	content  string
	material materials.Material
	project  domain.Project
}

// planMerge 只读：算出合回内容并校验。漂移还是 open 时才拒绝"手改在项目规范之外"；已选合回后重试不再拒绝。
func planMerge(tx *store.Tx, d driftRow) (p mergePlan, err error) {
	rec, ok, err := renderByHash(tx, d.repo, d.renderHash)
	if err != nil || !ok {
		return p, errors.Join(err, notFoundIf(!ok, "render", d.renderHash))
	}
	if p.project, _, err = projects.ByRemote(tx, d.repo); err != nil {
		return p, err
	}
	if p.content, err = mergedProject(rec, d.content); err != nil {
		return p, err
	}
	if strings.TrimSpace(p.content) == "" {
		return p, invalid("nothing to merge back: the edited file has no project guideline content")
	}
	ms, err := materials.List(tx, p.project.ID)
	if err != nil {
		return p, err
	}
	for _, m := range ms {
		if m.Kind == materials.KindGuideline && m.Project == p.project.ID && m.Location == "" {
			p.material = m
		}
	}
	if d.state == driftOpen && p.material.ID != "" && p.material.CurrentRev != "" {
		if _, cur, err := materials.Show(tx, p.material.ID, ""); err != nil {
			return p, err
		} else if normalize(cur.Content) == normalize(p.content) {
			return p, invalid("the hand edit is outside the project guideline (global rules or generated sections): change it in the guideline it comes from")
		}
	}
	return p, nil
}

// commitMerge 经 material.commit 提交 proposed 修订。始终以内核身份提交（幂等键按主体计，Resolve 与回答后补提必须同主体才不重复）；
// 谁做的选择已记在决策结果里，授权在回答命令里已过。
func (s *Service) commitMerge(ctx context.Context, item string, p mergePlan) (materials.Result, error) {
	commit := map[string]any{"content": p.content, "reason": "合回 AGENTS.md 手改内容（" + item + "）"}
	if p.material.ID != "" {
		commit["id"], commit["based_on"] = p.material.ID, p.material.CurrentRev
	} else {
		commit["project"], commit["kind"], commit["title"] = p.project.ID, materials.KindGuideline, p.project.Name+" 规范"
	}
	b, _ := json.Marshal(commit)
	var r materials.Result
	res, err := s.x.Do(ctx, command.Request{Identity: system, Command: "material.commit", Input: b, IdemKey: "agentsmd-merge:" + item})
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(res.Value, &r)
}

// onDecision 在决策回答的同一事务里落实漂移事项的所选项（Web 回答、POST /items/{id}/decide、delegate 代答都经 decisions.Answer）：
// 覆盖与忽略记下选择；合回先校验（不合格则整个回答回滚），提交后由 flushMerges 补提 proposed 修订。
func (s *Service) onDecision(c *command.Ctx) error {
	for _, e := range c.Events() {
		if e.Type != domain.EvDecisionResolved {
			continue
		}
		d, ok, err := findDrift(c.Tx, e.Object)
		if err != nil || !ok || d.state != driftOpen {
			if err != nil {
				return err
			}
			continue
		}
		it, err := items.Get(c.Tx, e.Object)
		if err != nil {
			return err
		}
		if it.Ask == nil || it.Ask.Resolution == nil || !slices.Contains([]string{ChoiceMerge, ChoiceOverride, ChoiceIgnore}, it.Ask.Resolution.Choice) {
			continue
		}
		choice := it.Ask.Resolution.Choice
		if choice == ChoiceMerge {
			if _, err := planMerge(c.Tx, d); err != nil {
				return err
			}
			s.mu.Lock()
			s.merging = append(s.merging, d.item)
			s.mu.Unlock()
		}
		if _, err := c.Tx.Exec("UPDATE agentsmd_drift SET state = ? WHERE item = ?", choice, d.item); err != nil {
			return err
		}
	}
	return nil
}

// flushMerges 在命令提交后为已选"合回"的漂移补提修订。失败交给 OnError：重新用同样的选择调用 Resolve 可重试，幂等键保证不重复。
func (s *Service) flushMerges() {
	s.mu.Lock()
	pending := s.merging
	s.merging = nil
	s.mu.Unlock()
	for _, item := range pending {
		var (
			d driftRow
			p mergePlan
		)
		err := s.x.Store().View(context.Background(), func(tx *store.Tx) (err error) {
			if d, err = driftByItem(tx, item); err != nil {
				return err
			}
			if d.state != driftMerged { // 回答所在事务回滚了
				return nil
			}
			p, err = planMerge(tx, d)
			return err
		})
		if err == nil && d.state == driftMerged {
			_, err = s.commitMerge(context.Background(), item, p)
		}
		if err != nil {
			s.opt.OnError(fmt.Errorf("agentsmd: merge back drift %s: %w", item, err))
		}
	}
}

// Resolve 落实本人对漂移的选择。覆盖与忽略一条命令完成；合回先在项目规范上提 proposed 修订，
// 规范生效属 web_only，所以仍要本人确认，确认后（revision.confirm）自动重新渲染。
// 合回的修订经 material.commit 提交，用漂移事项 id 作幂等键：中途失败后用同样的选择重试，不会重复提交。
func (s *Service) Resolve(ctx context.Context, who command.Identity, in ResolveIn) (ResolveOut, error) {
	var (
		d    driftRow
		plan mergePlan
	)
	err := s.x.Store().View(ctx, func(tx *store.Tx) (err error) {
		if d, err = driftByItem(tx, in.Item); err != nil || in.Choice != ChoiceMerge {
			return err
		}
		plan, err = planMerge(tx, d)
		return err
	})
	if err != nil {
		return ResolveOut{}, err
	}
	if d.state == driftOpen {
		b, _ := json.Marshal(in)
		if _, err := s.x.Do(ctx, command.Request{Identity: who, Command: "agents.resolve", Input: b}); err != nil {
			return ResolveOut{}, err
		}
	} else if d.state != in.Choice {
		return ResolveOut{}, conflict("this drift was already decided: "+d.state, "the first choice stands; read the item")
	}
	out := ResolveOut{Item: in.Item, Choice: in.Choice}
	if in.Choice != ChoiceMerge {
		return out, nil
	}
	r, err := s.commitMerge(ctx, in.Item, plan)
	if err != nil {
		return out, err
	}
	out.Material, out.Revision = r.Material, r.Revision
	return out, nil
}

func notFoundIf(cond bool, what, id string) error {
	if !cond {
		return nil
	}
	return domain.NewError(domain.CodeNotFound, what+" "+id+" does not exist", "re-run the check")
}

// ---- 触发与读取 ----

// Trigger 在来源看到仓库默认分支上的提交时调用：改动路径含根目录 AGENTS.md 就读默认分支上的文件，
// 走与 apa agents check 同一条判定（漂移则生成同样的 needs_input 事项）。不相关的提交返回 nil。
func (s *Service) Trigger(ctx context.Context, repo string, changed []string) (*Verdict, error) {
	if !slices.ContainsFunc(changed, isFile) {
		return nil, nil
	}
	if s.opt.Fetch == nil {
		return nil, errors.New("agentsmd: Options.Fetch is not configured")
	}
	var reg domain.Repo
	if err := s.x.Store().View(ctx, func(tx *store.Tx) error {
		_, r, ok, err := projects.RepoOf(tx, repo)
		if err == nil && !ok {
			err = notFound("repository", repo)
		}
		reg = r
		return err
	}); err != nil {
		return nil, err
	}
	content, exists, err := s.opt.Fetch(ctx, reg.Remote)
	if err != nil {
		return nil, fmt.Errorf("agentsmd: read %s on the default branch of %s: %w", FileName, reg.Remote, err)
	}
	in := checkIn{Repo: reg.Remote, Host: "default-branch", Content: &content, Missing: !exists}
	if !exists {
		in.Content = nil
	}
	b, _ := json.Marshal(in)
	res, err := s.x.Do(ctx, command.Request{Identity: system, Command: "agents.check", Input: b})
	if err != nil {
		return nil, err
	}
	var vd Verdict
	return &vd, json.Unmarshal(res.Value, &vd)
}

// View 是 GET /agents-md?repo 的内容：最新渲染，加最近一次检查（从未检查过为 nil）。
type View struct {
	Content    string     `json:"content"`
	Hash       string     `json:"hash"`
	RenderedAt int64      `json:"rendered_at"`
	LastCheck  *LastCheck `json:"last_check"`
}

// LastCheck 是同一仓库最近一次检查。
type LastCheck struct {
	State     string `json:"state"`
	FileHash  string `json:"file_hash"`
	Host      string `json:"host"`
	CheckedAt int64  `json:"checked_at"`
}

// Get 读取仓库的最新渲染与最近一次检查；仓库从未渲染过返回 not_found。
func (s *Service) Get(ctx context.Context, repo string) (View, error) {
	var v View
	err := s.x.Store().View(ctx, func(tx *store.Tx) error {
		// 查不到登记项时按原写法读：仓库从项目移除后，历史渲染仍可查。
		if _, reg, ok, err := projects.RepoOf(tx, repo); err != nil {
			return err
		} else if ok {
			repo = reg.Remote
		}
		rec, ok, err := latestRender(tx, repo)
		if err != nil || !ok {
			return errors.Join(err, notFoundIf(!ok, "render of repository", repo))
		}
		v = View{Content: rec.Content, Hash: rec.Hash, RenderedAt: rec.RenderedAt}
		r, err := tx.Query("SELECT state, file_hash, host, checked_at FROM agentsmd_check WHERE repo = ?", repo)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			v.LastCheck = &LastCheck{}
			return r.Scan(&v.LastCheck.State, &v.LastCheck.FileHash, &v.LastCheck.Host, &v.LastCheck.CheckedAt)
		}
		return r.Err()
	})
	return v, err
}

// OpenDrift 是一条尚未决定的漂移：哪个仓库、对应的 needs_input 事项。
type OpenDrift struct{ Repo, Item string }

// OpenDrifts 返回所有未决定的漂移，按事项 id 排序；健康视图只读。
func OpenDrifts(tx *store.Tx) (out []OpenDrift, err error) {
	r, err := tx.Query("SELECT repo, item FROM agentsmd_drift WHERE state = ? ORDER BY item", driftOpen)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for r.Next() {
		var d OpenDrift
		if err := r.Scan(&d.Repo, &d.Item); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, r.Err()
}
