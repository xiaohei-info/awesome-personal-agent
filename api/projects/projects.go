// Package projects 是项目分组的 HTTP 路由（附录 B.2、设计 §12.1、§14.7）：
//
//	GET/POST /projects        GET/PATCH /projects/{id}     （读取带项目卡片：各阶段条数与最近活动）
//	POST /projects/{id}/repos
//	GET /projects/resolve?path&host[&project]
//
// 路由只做传输：解码、把请求交给内核命令（授权、校验、事件都在 kernel/projects 与命令管线）、渲染结果。
// allowed_executors 不能经这里写入（§14.7），只有 Web 的 settings 路由可改。
package projects

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/readscope"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	km "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// IdempotencyHeader 是写请求可选的幂等键头。
const IdempotencyHeader = "Idempotency-Key"

const allowedExecutorsField = "allowed_executors"

type api struct {
	st *store.Store
	x  *command.Executor
}

// Routes 返回项目路由分组；x 须已由 kernel/projects.Register 登记命令。经 core.Deps.Groups 挂载。
func Routes(st *store.Store, x *command.Executor) core.RouteGroup {
	a := &api{st, x}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/projects", Handler: readable(a.list)})
		r.Handle(core.Route{Method: "POST", Path: "/projects", Handler: a.create})
		r.Handle(core.Route{Method: "GET", Path: "/projects/resolve", Handler: readable(a.resolve)})
		r.Handle(core.Route{Method: "GET", Path: "/projects/{id}", Handler: readable(a.get)})
		r.Handle(core.Route{Method: "PATCH", Path: "/projects/{id}", Handler: a.patch})
		r.Handle(core.Route{Method: "POST", Path: "/projects/{id}/repos", Handler: a.repos})
	}
}

// readable：读项目对 observer 及以上开放；ext 与 webhook 凭据另有用途，不能读账本（与 api/items 一致）。
func readable(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
			return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read projects", "use an owner or agent credential")
		}
		return h(w, r, p)
	}
}

func invalid(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

// body 读出请求体里的 JSON 对象。
func body(r *http.Request) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := core.DecodeJSON(r, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, invalid("request body must be a JSON object", "send a JSON object")
	}
	return m, nil
}

// run 以请求的凭据执行一条内核命令。
func (a *api) run(r *http.Request, p *core.Principal, cmd string, in map[string]json.RawMessage) (command.Result, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return command.Result{}, err
	}
	return a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: cmd, Input: raw, IdemKey: r.Header.Get(IdempotencyHeader), WebURL: p.WebURL})
}

func (a *api) view(r *http.Request, id string) (p domain.Project, err error) {
	err = a.st.View(r.Context(), func(tx *store.Tx) (e error) { p, e = kp.Get(tx, id); return })
	return
}

// reply 把命令结果指向的项目读回并渲染。
func (a *api) reply(w http.ResponseWriter, r *http.Request, status int, res command.Result) error {
	var ref struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &ref); err != nil {
		return err
	}
	p, err := a.view(r, ref.ID)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, status, p)
}

func webOnlyExecutors() error {
	return domain.NewError(domain.CodeInvalidRequest, "allowed_executors is not part of creating a project", "create the project, then set it with PUT /v1/settings/projects/{id}/allowed-executors")
}

// Card 是项目卡片（§12.1）：现算的各阶段事项条数与最近活动日；单个项目另带最近变更的事项与材料条数。
// 外部钥匙只数它读得到的事项（读取闸，§14.14）。
type Card struct {
	Counts       map[domain.Phase]int `json:"counts"`
	LastActivity string               `json:"last_activity,omitempty"` // 本机日期 YYYY-MM-DD
	Recent       []RecentItem         `json:"recent,omitempty"`
	Materials    *int                 `json:"materials,omitempty"`
}

// RecentItem 是卡片里的一条最近变更（已放弃的不列）。
type RecentItem struct {
	ID      string       `json:"id"`
	Title   string       `json:"title"`
	Phase   domain.Phase `json:"phase"`
	Updated string       `json:"updated"`
}

// recentRows 是单个项目卡片里最近变更的条数。
const recentRows = 5

// cards 按项目汇总 its（已过读取闸）；withRecent 时带最近变更。
func cards(its []domain.Item, withRecent bool) map[string]*Card {
	out := map[string]*Card{}
	slices.SortFunc(its, func(a, b domain.Item) int { return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(a.ID, b.ID)) })
	for _, it := range its {
		c := out[it.Project]
		if c == nil {
			c = &Card{Counts: map[domain.Phase]int{}, LastActivity: it.UpdatedAt.In(time.Local).Format("2006-01-02")}
			out[it.Project] = c
		}
		c.Counts[it.Phase]++
		if withRecent && len(c.Recent) < recentRows && it.Phase != domain.PhaseDropped {
			c.Recent = append(c.Recent, RecentItem{it.ID, it.Title, it.Phase, it.UpdatedAt.In(time.Local).Format("2006-01-02")})
		}
	}
	return out
}

func empty() *Card { return &Card{Counts: map[domain.Phase]int{}} }

func (a *api) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var (
		ps []domain.Project
		cs map[string]*Card
	)
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if ps, err = kp.List(tx); err != nil {
			return err
		}
		its, err := kitems.List(tx, kitems.Filter{})
		if err == nil {
			its, _, err = readscope.New(tx, p.Caller).Items(its)
		}
		cs = cards(its, false)
		return err
	}); err != nil {
		return err
	}
	if ps == nil {
		ps = []domain.Project{}
	}
	out := map[string]*Card{}
	for _, pr := range ps {
		if out[pr.ID] = cs[pr.ID]; out[pr.ID] == nil {
			out[pr.ID] = empty()
		}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"projects": ps, "cards": out})
}

func (a *api) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var out struct {
		domain.Project
		Card *Card `json:"card"`
	}
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if out.Project, err = kp.Get(tx, r.PathValue("id")); err != nil {
			return err
		}
		its, err := kitems.List(tx, kitems.Filter{Project: out.Project.ID})
		if err != nil {
			return err
		}
		if its, _, err = readscope.New(tx, p.Caller).Items(its); err != nil {
			return err
		}
		if out.Card = cards(its, true)[out.Project.ID]; out.Card == nil {
			out.Card = empty()
		}
		ms, err := km.List(tx, out.Project.ID)
		n := len(ms)
		out.Card.Materials = &n
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

func (a *api) create(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in, err := body(r)
	if err != nil {
		return err
	}
	if _, ok := in[allowedExecutorsField]; ok {
		return webOnlyExecutors()
	}
	res, err := a.run(r, p, "project.create", in)
	if err != nil {
		return err
	}
	return a.reply(w, r, http.StatusCreated, res)
}

func (a *api) patch(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	in, err := body(r)
	if err != nil {
		return err
	}
	if ex, ok := in[allowedExecutorsField]; ok {
		return a.rejectExecutors(r, p, id, ex)
	}
	in["id"], _ = json.Marshal(id)
	res, err := a.run(r, p, "project.update", in)
	if err != nil {
		return err
	}
	return a.reply(w, r, http.StatusOK, res)
}

// rejectExecutors：这条路由从不写 allowed_executors。非 Web 调用者的尝试交给内核的设置命令判定，
// 以得到 §14.7 规定的退出码（delegate 20 并留拒绝事件，其余 4）；Web 本人则被指向 settings 路由。
func (a *api) rejectExecutors(r *http.Request, p *core.Principal, id string, executors json.RawMessage) error {
	if !p.Web {
		in := map[string]json.RawMessage{"executors": executors}
		in["id"], _ = json.Marshal(id)
		if _, err := a.run(r, p, "project.allowed_executors.set", in); err != nil {
			return err
		}
	}
	return invalid("allowed_executors is not part of the project update", "change it through /v1/settings/projects/"+id+"/allowed-executors in Web")
}

type repoReq struct {
	Op     string `json:"op"`
	Remote string `json:"remote"`
	Host   string `json:"host"`
	Path   string `json:"path"`
}

// repoOps 把 op 映射到内核命令；谁能调用由命令的授权动作决定（远程地址 delegate，本机检出 worker）。
var repoOps = map[string]string{
	"add_remote":      "project.repo.add",
	"remove_remote":   "project.repo.remove",
	"set_checkout":    "project.checkout.set",
	"remove_checkout": "project.checkout.remove",
}

func (a *api) repos(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var req repoReq
	if err := core.DecodeJSON(r, &req); err != nil {
		return err
	}
	cmd, ok := repoOps[req.Op]
	if !ok {
		return invalid("op must be one of add_remote, remove_remote, set_checkout, remove_checkout", "fix op and retry")
	}
	in := map[string]json.RawMessage{}
	for k, v := range map[string]string{"id": r.PathValue("id"), "remote": req.Remote, "host": req.Host, "path": req.Path} {
		if v != "" {
			in[k], _ = json.Marshal(v)
		}
	}
	res, err := a.run(r, p, cmd, in)
	if err != nil {
		return err
	}
	return a.reply(w, r, http.StatusOK, res)
}

func (a *api) resolve(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	q := r.URL.Query()
	s := kp.Scope{Flag: q.Get("project"), Cwd: q.Get("path"), Host: q.Get("host")}
	if s.Cwd != "" && s.Host == "" {
		return invalid("host is required with path", "send the machine's host name")
	}
	var proj *domain.Project
	err := a.st.View(r.Context(), func(tx *store.Tx) error {
		id, err := kp.Resolve(tx, s)
		if err != nil || id == "" {
			return err
		}
		p, err := kp.Get(tx, id)
		proj = &p
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"project": proj})
}
