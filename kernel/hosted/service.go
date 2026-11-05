package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
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
	// Now 默认 time.Now。
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
		o.Now = time.Now
	}
	return &Service{x: x, opt: o, hit: map[string]cached{}}, nil
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
		if r.Remote != e.Repo {
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
	err := s.x.Store().Update(ctx, func(tx *store.Tx) error {
		e, err := Get(tx, id)
		if err != nil {
			return err
		}
		v = View{Entry: e, Status: StatusReading}
		if force {
			s.mu.Lock()
			delete(s.hit, id) // 刷新信号：旧结果作废，页面轮询到新结果前显示"读取中"
			s.mu.Unlock()
		} else if c, ok := s.lookup(e); ok {
			v = c
			return nil
		}
		busy, err := proc.InFlight(tx, id)
		if err != nil || busy {
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
			Explicit: force,
		}, s.opt.Now())
		return err
	})
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
		ttl, refresh = s.opt.TTL, refreshIn{ID: id, Commit: out.Commit}
	case exit == exitPathMissing:
		v.Reason = "path not found on the default branch"
		refresh = refreshIn{ID: id, PathMissing: true}
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
// delegate 开放，worker 与运行凭据必须带自己权限范围内的 parent。委派失败时丢弃刚建的父事项，不留孤儿。
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
		r, err := s.x.Do(ctx, command.Request{Identity: who, Command: "item.create", Input: b, Owner: s.opt.Owner, Chain: command.Chain{Mode: command.ChainNew}})
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
	chain := command.Chain{Mode: command.ChainNew}
	if level != authz.LevelWeb {
		chain = command.Chain{Mode: command.ChainInherit, From: out.Parent}
	}
	r, err := s.x.Do(ctx, command.Request{Identity: who, Command: "delegate.create", Input: b, Owner: s.opt.Owner, Chain: chain})
	if err != nil {
		if out.ParentCreated {
			drop, _ := json.Marshal(map[string]any{"id": out.Parent, "to": domain.PhaseDropped, "reason": "delegation failed: " + err.Error()})
			if _, derr := s.x.Do(ctx, command.Request{Identity: who, Command: "item.transition", Input: drop, Owner: s.opt.Owner}); derr != nil {
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
	return out, nil
}

func reasonOr(r, def string) string {
	if blank(r) {
		return def
	}
	return r
}
