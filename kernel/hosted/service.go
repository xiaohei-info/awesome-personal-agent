package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 读取器的退出码沿用 Source 契约（§9.2）：3 是永久失败，默认分支上已没有该路径。
const exitPathMissing = 3

// Status 是 GET /materials/{id}/content 的状态。
type Status string

const (
	StatusReading    Status = "reading"    // 已入队或在途，页面轮询
	StatusReady      Status = "ready"      // 有内容和 commit
	StatusUnreadable Status = "unreadable" // 只显示登记信息和"暂不可读"，附原因
)

// View 是一次读取的响应。读取失败不是错误：Status 为 unreadable，Reason 说明，登记信息照常给出。
type View struct {
	Entry   Entry  `json:"entry"`
	Status  Status `json:"status"`
	Content string `json:"content,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Options 注入本服务读取但不拥有的东西。
type Options struct {
	// Reader 是只读读取器的 argv（按 Source 契约登记的子进程），必填；凭据由读取器自持，内核不读。
	Reader []string
	// Host 是本 APA 主机在项目 repos 检出里的名字；项目有本机检出时读检出的默认分支，否则读远程。
	Host string
	// Owner 是本人的执行者 id：没带 parent 的修改在材料所在项目下先建的 task 事项归他。
	Owner string
	// Kick 在入队后被调用，通知调度器拉起（通常是 Dispatcher.Dispatch）；可空。
	Kick func()
	// OnError 接收读取结果回写登记失败的错误，必填：失败时 commit 与路径状态会停在旧值，不能静默。
	OnError func(material string, err error)
	// TTL 是读取成功的缓存时长，默认 5 分钟；FailTTL 是读取失败后不再重试的时长，默认 30 秒。
	TTL, FailTTL time.Duration
	// Timeout 是单次读取的秒数上限，默认 30。
	Timeout int
	// Now 默认 clock.Now。
	Now func() time.Time
}

type cached struct {
	repo, path string
	view       View
	expires    time.Time
}

// Service 提供托管型材料的读取与修改。缓存只在进程内，不进账本，重启后重新读取，所以不产生正文副本。
type Service struct {
	x   *command.Executor
	opt Options
	mu  sync.Mutex
	hit map[string]cached
	// pending 是已入队、结果还没被 OnResult 处理完的材料。Run 在 OnResult 之前就已是终态，
	// 这段空窗里账本不报在途、缓存也还没有，不拦住的话轮询会多拉起一次读取。
	pending map[string]bool
}

// New 创建服务。Reader 与 OnError 必填。
func New(x *command.Executor, o Options) (*Service, error) {
	if len(o.Reader) == 0 || o.Reader[0] == "" {
		return nil, invalid("hosted: Options.Reader is required")
	}
	if o.OnError == nil {
		return nil, invalid("hosted: Options.OnError is required")
	}
	if o.TTL <= 0 {
		o.TTL = 5 * time.Minute
	}
	if o.FailTTL <= 0 {
		o.FailTTL = 30 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 30
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	s := &Service{x: x, opt: o, hit: map[string]cached{}, pending: map[string]bool{}}
	x.OnCommit(s.onCommit)
	return s, nil
}

// onCommit 是最近 commit 的刷新时机（§12.3）：登记后读一次；apa agents check 刷新该仓库登记的全部托管文档；
// 委派回执 done 时刷新子事项及其父事项以 output 关联的托管文档（修改托管文档走委派，见 RequestChange）。
// 刷新是异步读取，读到的 commit 或"路径已消失"经 material.refresh 落账。
func (s *Service) onCommit(c command.Commit) {
	var ids []string
	err := s.x.Store().View(context.Background(), func(tx *store.Tx) (err error) {
		ids, err = refreshTargets(tx, c)
		return
	})
	if err != nil {
		s.opt.OnError(c.Object, err)
		return
	}
	for _, id := range ids {
		if _, err := s.Refresh(context.Background(), id); err != nil {
			s.opt.OnError(id, err)
		}
	}
}

func refreshTargets(tx *store.Tx, c command.Commit) ([]string, error) {
	switch c.Command {
	case "material.add":
		var r Result
		if json.Unmarshal(c.Value, &r) != nil || r.Material == "" { // 链路超限只记录不执行，没有结果
			return nil, nil
		}
		return []string{r.Material}, nil
	case "agents.check":
		var in struct {
			Repo string `json:"repo"`
		}
		_ = json.Unmarshal(c.Input, &in) // 管线已按同一结构解析过
		es, err := List(tx, "")
		var ids []string
		for _, e := range es {
			if projects.SameRepo(e.Repo, in.Repo) {
				ids = append(ids, e.Material.ID)
			}
		}
		return ids, err
	case "delegate.report":
		child, err := items.Get(tx, c.Object)
		if err != nil || child.Phase != domain.PhaseDone {
			return nil, err
		}
		from := []string{child.ID}
		if p, ok, err := items.ParentOf(tx, child.ID); err != nil {
			return nil, err
		} else if ok {
			from = append(from, p)
		}
		var ids []string
		for _, f := range from {
			rels, err := items.Relations(tx, f)
			if err != nil {
				return nil, err
			}
			for _, r := range rels {
				if r.From != f || r.Type != domain.RelOutput || slices.Contains(ids, r.To) {
					continue
				}
				if ok, err := isHosted(tx, r.To); err != nil {
					return nil, err
				} else if ok {
					ids = append(ids, r.To)
				}
			}
		}
		return ids, nil
	}
	return nil, nil
}

func isHosted(tx *store.Tx, id string) (bool, error) {
	r, err := tx.Query("SELECT 1 FROM hosted_material WHERE material = ?", id)
	if err != nil {
		return false, err
	}
	defer r.Close()
	found := r.Next()
	return found, r.Err()
}

// system 是读取结果回写登记时的内部身份。
var system = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "kernel:hosted", Via: domain.ViaClock}, Level: authz.LevelWorker}

// Content 返回材料内容：缓存有效直接给；已有在途读取就复用并回"读取中"；否则入队一个 reader Run 回"读取中"。
func (s *Service) Content(ctx context.Context, id string) (View, error) {
	return s.read(ctx, id, false)
}

// Refresh 忽略缓存重新读一次，读到的 commit 或"路径已消失"会回写登记。来源事件、回执和 agents check 用它。
func (s *Service) Refresh(ctx context.Context, id string) (View, error) { return s.read(ctx, id, true) }

type readerIn struct {
	SourceID string       `json:"source_id"`
	Config   readerConfig `json:"config"`
}

type readerConfig struct {
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Checkout string `json:"checkout,omitempty"`
}

func (s *Service) checkout(tx *store.Tx, e Entry) (string, error) {
	p, err := projects.Get(tx, e.Material.Project)
	if err != nil {
		return "", err
	}
	for _, r := range p.Repos {
		if !projects.SameRepo(r.Remote, e.Repo) {
			continue
		}
		for _, c := range r.Checkouts {
			if c.Host == s.opt.Host {
				return c.Path, nil
			}
		}
	}
	return "", nil
}

func (s *Service) lookup(e Entry) (View, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.hit[e.Material.ID]
	if !ok || c.repo != e.Repo || c.path != e.Path || !s.opt.Now().Before(c.expires) {
		delete(s.hit, e.Material.ID)
		return View{}, false
	}
	c.view.Entry = e
	return c.view, true
}

func (s *Service) read(ctx context.Context, id string, force bool) (View, error) {
	var v View
	var queued bool
	err := s.x.Store().Update(ctx, func(tx *store.Tx) error {
		e, err := Get(tx, id)
		if err != nil {
			return err
		}
		v = View{Entry: e, Status: StatusReading}
		s.mu.Lock()
		applying := s.pending[id] // 必须先于查缓存：OnResult 先放缓存、后清 pending
		if force {
			delete(s.hit, id) // 刷新信号：旧结果作废，页面轮询到新结果前显示"读取中"
		}
		s.mu.Unlock()
		if !force {
			if c, ok := s.lookup(e); ok {
				v = c
				return nil
			}
		}
		busy, err := proc.InFlight(tx, id)
		if err != nil || busy || applying {
			return err
		}
		co, err := s.checkout(tx, e)
		if err != nil {
			return err
		}
		stdin, _ := json.Marshal(readerIn{SourceID: id, Config: readerConfig{Repo: e.Repo, Path: e.Path, Checkout: co}})
		_, err = proc.Enqueue(tx, proc.Request{
			Run:      domain.Run{Category: domain.RunReader, Target: id, TimeoutSec: s.opt.Timeout, Fingerprint: fingerprint(e)},
			Spec:     proc.Spec{Command: s.opt.Reader, Stdin: stdin},
			Explicit: !force, // 本人在 Web 打开是显式读取，在短任务队列内优先（§6.9）；登记、回执、agents check 的刷新不是
		}, s.opt.Now())
		if err == nil {
			s.mu.Lock() // 在事务内置位：提交后 Run 才可能被派发，OnResult 不会先于它清除
			s.pending[id] = true
			s.mu.Unlock()
			queued = true
		}
		return err
	})
	if err != nil && queued { // 提交失败：没有 Run，不能留着 pending
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}
	if err == nil && v.Status == StatusReading && s.opt.Kick != nil {
		s.opt.Kick()
	}
	return v, err
}

// fingerprint 记下这次读取的是哪个仓库路径；登记在读取期间被修正时，旧结果不能回写。
func fingerprint(e Entry) string { return e.Repo + "\n" + e.Path }

type readerOut struct {
	Commit  string `json:"commit"`
	Branch  string `json:"branch"`
	Content string `json:"content"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// lastJSON 取 stdout 末段里最后一行 JSON；读取器的契约是 stdout 只有一行。
func lastJSON(b []byte) (out readerOut, ok bool) {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if len(lines) == 0 {
		return out, false
	}
	return out, json.Unmarshal(lines[len(lines)-1], &out) == nil
}

// OnResult 接收调度器交回的 reader Run 结果（接到 proc.Options.OnResult）：缓存读取结果，
// 回写最近 commit，路径消失时经 material.refresh 生成 needs_input。其他类别的 Run 忽略。
func (s *Service) OnResult(res proc.Result) {
	if res.Run.Category != domain.RunReader {
		return
	}
	id := res.Run.Target
	defer func() { // 放在缓存写入之后（defer 最后执行），见 pending 注释
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	var e Entry
	if err := s.x.Store().View(context.Background(), func(tx *store.Tx) (err error) { e, err = Get(tx, id); return }); err != nil {
		s.opt.OnError(id, err)
		return
	}
	if res.Run.Fingerprint != fingerprint(e) {
		return // 登记已被修正，这次读的是旧路径
	}
	out, parsed := lastJSON(res.Stdout)
	exit := -1
	if res.Run.ExitCode != nil {
		exit = *res.Run.ExitCode
	}
	v, ttl := View{Entry: e, Status: StatusUnreadable}, s.opt.FailTTL
	var refresh refreshIn
	switch {
	case res.Run.State == domain.RunSucceeded && parsed && out.Commit != "":
		v = View{Entry: e, Status: StatusReady, Content: out.Content, Commit: out.Commit, Branch: out.Branch}
		ttl, refresh = s.opt.TTL, refreshIn{ID: id, Commit: out.Commit, Path: e.Path}
	case exit == exitPathMissing:
		v.Reason = "path not found on the default branch"
		refresh = refreshIn{ID: id, PathMissing: true, Path: e.Path}
	default:
		v.Reason = res.Reason
		if parsed && out.Error != nil {
			v.Reason = out.Error.Message
		}
		if v.Reason == "" {
			v.Reason = "reader failed"
		}
	}
	// 先回写登记再放缓存：页面看到结果时，最近 commit 与修正事项已经落账。
	if refresh.ID != "" {
		in, _ := json.Marshal(refresh)
		if _, err := s.x.Do(context.Background(), command.Request{Identity: system, Command: "material.refresh", Input: in,
			Chain: command.Chain{Mode: command.ChainKeep, From: id}}); err != nil {
			s.opt.OnError(id, err)
		}
	}
	s.mu.Lock()
	s.hit[id] = cached{repo: e.Repo, path: e.Path, view: v, expires: s.opt.Now().Add(ttl)}
	s.mu.Unlock()
}

// ---- 修改：生成委派子事项，不在 APA 内改仓库文档（§12.3） ----

// ChangeIn 是 POST /materials/{id}/changes 的输入。
type ChangeIn struct {
	ID       string `json:"id"`
	Assignee string `json:"assignee"`
	Request  string `json:"request"` // 修改要求，必填
	Parent   string `json:"parent,omitempty"`
	Reason   string `json:"reason,omitempty"`

	// 请求元数据，不是请求体字段。IdemKey 给两步命令（item.create、delegate.create）各用一次——幂等按命令名分键，
	// 重放返回首次的父事项和子事项；Evidence 是 delegate 代答所需的本人原话。
	// shortcut: 委派失败后父事项已丢弃，同键重试会重放到这个已丢弃的父事项并再次失败；换新键重试即可。
	IdemKey  string `json:"-"`
	Evidence string `json:"-"`
}

// ChangeOut 是生成的父事项与委派子事项。
type ChangeOut struct {
	Parent        string `json:"parent"`
	Child         string `json:"child"`
	ParentCreated bool   `json:"parent_created"` // 没带 parent 时由内核先建
}

type id struct {
	ID string `json:"id"`
}

// RequestChange 为托管文档的修改生成委派子事项（带 project、仓库、路径、修改要求；允许的执行者由 delegate.create 校验）。
// 父事项来源：带 parent 时须属于材料所在项目；不带时内核先建一个 task 事项（open，归本人）。不带 parent 只对 Web 登录和
// delegate 开放，worker 与运行凭据必须带自己权限范围内的 parent。委派失败时丢弃刚建的父事项，不留孤儿；
// 委派成功后父事项以 output 关联材料（以调用者身份经 item.relate）。
func (s *Service) RequestChange(ctx context.Context, who command.Identity, in ChangeIn) (ChangeOut, error) {
	if blank(in.Request) {
		return ChangeOut{}, invalid("request is required: say what to change")
	}
	if blank(in.Assignee) {
		return ChangeOut{}, invalid("assignee is required")
	}
	var e Entry
	var level authz.Level
	err := s.x.Store().View(ctx, func(tx *store.Tx) (err error) {
		c, err := who.Authenticate(tx)
		if err != nil {
			return err
		}
		level = c.Level
		if e, err = Get(tx, in.ID); err != nil {
			return err
		}
		if in.Parent == "" {
			return nil
		}
		p, err := items.Get(tx, in.Parent)
		if err != nil {
			return err
		}
		if p.Project != e.Material.Project {
			return invalid("parent must belong to the project of the material")
		}
		return nil
	})
	if err != nil {
		return ChangeOut{}, err
	}
	out := ChangeOut{Parent: in.Parent}
	if in.Parent == "" {
		if level != authz.LevelWeb && level != authz.LevelDelegate {
			return out, domain.NewError(domain.CodeInvalidRequest, "parent is required for this credential",
				"pass a parent item within your scope")
		}
		reason := reasonOr(in.Reason, "change requested for hosted material "+e.Material.Title)
		b, _ := json.Marshal(map[string]any{"kind": domain.KindTask, "phase": domain.PhaseOpen, "title": "修改文档：" + e.Material.Title,
			"summary": e.Repo + ":" + e.Path, "project": e.Material.Project, "assignee": s.opt.Owner, "reason": reason})
		r, err := s.x.Do(ctx, command.Request{Identity: who, Command: "item.create", Input: b, Owner: s.opt.Owner,
			IdemKey: in.IdemKey, Evidence: in.Evidence})
		if err != nil {
			return out, err
		}
		var ref id
		if err := json.Unmarshal(r.Value, &ref); err != nil {
			return out, err
		}
		out.Parent, out.ParentCreated = ref.ID, true
	}
	body := "仓库: " + e.Repo + "\n路径: " + e.Path + "\n\n修改要求:\n" + in.Request +
		"\n\n在仓库开分支提 PR，评审在 PR；合并后回执附 PR 作证据。"
	b, _ := json.Marshal(map[string]any{"parent": out.Parent, "assignee": in.Assignee, "title": "修改文档：" + e.Material.Title,
		"summary": e.Repo + ":" + e.Path, "body": body, "project": e.Material.Project, "reason": reasonOr(in.Reason, "change hosted material")})
	// 链路深度由命令管线按调用者推定（§14.9）：本人在 Web 委派开始新链路，其余继承父事项。
	r, err := s.x.Do(ctx, command.Request{Identity: who, Command: "delegate.create", Input: b, Owner: s.opt.Owner,
		IdemKey: in.IdemKey, Evidence: in.Evidence})
	if err != nil {
		if out.ParentCreated {
			drop, _ := json.Marshal(map[string]any{"id": out.Parent, "to": domain.PhaseDropped, "reason": "delegation failed: " + err.Error()})
			if _, derr := s.x.Do(ctx, command.Request{Identity: who, Command: "item.transition", Input: drop, Owner: s.opt.Owner, Evidence: in.Evidence}); derr != nil {
				err = errors.Join(err, derr)
			}
		}
		return ChangeOut{}, err
	}
	var child id
	if err := json.Unmarshal(r.Value, &child); err != nil {
		return out, err
	}
	out.Child = child.ID
	// 委派成功后父事项以 output 关联材料（§6.3）：两边可见，子事项回执 done 时据此刷新最近 commit。
	output := domain.Relation{Type: domain.RelOutput, From: out.Parent, To: e.Material.ID}
	var rels []domain.Relation
	if err := s.x.Store().View(ctx, func(tx *store.Tx) (err error) { rels, err = items.Relations(tx, out.Parent); return }); err != nil {
		return out, err
	}
	if !slices.Contains(rels, output) {
		rel, _ := json.Marshal(map[string]any{"from": output.From, "to": output.To, "type": output.Type, "reason": "change hosted material"})
		_, err = s.x.Do(ctx, command.Request{Identity: who, Command: "item.relate", Input: rel, Owner: s.opt.Owner,
			IdemKey: in.IdemKey, Evidence: in.Evidence})
	}
	return out, err
}

func reasonOr(r, def string) string {
	if blank(r) {
		return def
	}
	return r
}
