// Package connections 是连接（渠道、输出）的 HTTP 路由（附录 B.2，设计 §1.5、§7）：
//
//	GET    /connections?direction=        列出连接（凭据只回是否已设置）；token（observer 及以上）只得到现状摘要
//	GET    /connections/types?direction=  接入声明：选类型与渲染表单用
//	POST   /connections                   新建（connection.create）
//	PUT    /connections/{id}              改配置与凭据（connection.configure）
//	POST   /connections/{id}/rename       改名（connection.rename）
//	POST   /connections/{id}/enable       启用（connection.enable）
//	POST   /connections/{id}/disable      停用（connection.disable）
//	DELETE /connections/{id}              删除已停用的连接（connection.remove）
//	POST   /connections/test              测未保存的草稿：自检，send 为真时再发一条测试；不改账本
//	POST   /connections/{id}/test         对已保存的连接发一条测试，结果写进连接现状
//
// 路由里没有业务规则：校验、授权与事件都在内核命令；测试经渠道发送器调外呼调用器。除列表外全部只放行 Web 会话
// （与其他设置相同，任何 Bearer 凭据得 web_only，退出码 20）。列表对 token 只给 kconn.Summary（不含配置值、凭据与细节，§7.6），
// ext 与 webhook 密钥读不到。凭据永不出现在响应里。
package connections

import (
	"encoding/json"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kconn "github.com/xiaohei-info/awesome-personal-agent/kernel/connections"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type api struct {
	st    *store.Store
	x     *command.Executor
	cs    *kconn.Service
	call  *conn.Caller
	tests *channel.Sender
}

// Routes 返回路由分组；x 须已由 kconn.Service.Register 登记命令。
func Routes(st *store.Store, x *command.Executor, cs *kconn.Service, call *conn.Caller, s *channel.Sender) core.RouteGroup {
	a := &api{st, x, cs, call, s}
	return func(r *core.Router) {
		for _, rt := range []core.Route{
			{Method: "GET", Path: "/connections", Handler: a.list},
			{Method: "GET", Path: "/connections/types", Handler: web(a.types)},
			{Method: "POST", Path: "/connections", MaxBody: 16 << 10, Handler: web(a.create)},
			{Method: "PUT", Path: "/connections/{id}", MaxBody: 16 << 10, Handler: web(a.cmd(kconn.CmdConfigure))},
			{Method: "POST", Path: "/connections/{id}/rename", MaxBody: 1 << 10, Handler: web(a.cmd(kconn.CmdRename))},
			{Method: "POST", Path: "/connections/{id}/enable", MaxBody: 1 << 10, Handler: web(a.cmd(kconn.CmdEnable))},
			{Method: "POST", Path: "/connections/{id}/disable", MaxBody: 1 << 10, Handler: web(a.cmd(kconn.CmdDisable))},
			{Method: "DELETE", Path: "/connections/{id}", Handler: web(a.cmd(kconn.CmdRemove))},
			{Method: "POST", Path: "/connections/test", MaxBody: 16 << 10, Handler: web(a.testDraft)},
			{Method: "POST", Path: "/connections/{id}/test", MaxBody: 1 << 10, Handler: web(a.testSaved)},
		} {
			r.Handle(rt)
		}
	}
}

func web(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if !p.Web {
			return domain.NewError(domain.CodeWebOnly, "connections can only be managed by the owner in the Web UI", "ask the owner to change it in the Web settings")
		}
		return h(w, r, p)
	}
}

func (a *api) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if !p.Web && (p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook) {
		return domain.NewError(domain.CodePermissionDenied, "this credential kind cannot read connections", "use an owner or agent credential")
	}
	var cs []kconn.Connection
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { cs, err = a.cs.List(tx, r.URL.Query().Get("direction")); return }); err != nil {
		return err
	}
	if p.Web {
		return core.WriteJSON(w, http.StatusOK, map[string]any{"connections": cs})
	}
	out := make([]kconn.Summary, len(cs))
	for i, c := range cs {
		out[i] = c.Summary()
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (a *api) types(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	dir := r.URL.Query().Get("direction")
	out := []conn.Decl{}
	for _, d := range a.call.Decls() {
		if dir == "" || d.Direction == dir {
			out = append(out, d)
		}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"types": out})
}

func (a *api) do(r *http.Request, p *core.Principal, cmd string, in []byte) (json.RawMessage, error) {
	res, err := a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: cmd, Input: in, WebURL: p.WebURL})
	return res.Value, err
}

func (a *api) create(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in json.RawMessage
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	v, err := a.do(r, p, kconn.CmdCreate, in)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusCreated, v)
}

// cmd 把请求体（可为空）加上路径里的 id 交给命令；字段由命令严格校验。
func (a *api) cmd(name string) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		in := map[string]json.RawMessage{}
		if r.ContentLength != 0 && r.Method != http.MethodDelete {
			if err := core.DecodeJSON(r, &in); err != nil {
				return err
			}
			if in == nil {
				return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a JSON object")
			}
		}
		in["id"], _ = json.Marshal(r.PathValue("id"))
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		v, err := a.do(r, p, name, raw)
		if err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, v)
	}
}

type testResult struct {
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

func result(err error) testResult {
	if err == nil {
		return testResult{OK: true}
	}
	out := testResult{Problem: conn.ProblemOf(err), Detail: err.Error()}
	if p, ok := err.(*conn.Problem); ok {
		out.Detail = p.Detail
	}
	return out
}

type draftIn struct {
	Type    string            `json:"type"`
	ID      string            `json:"id,omitempty"` // 编辑已有连接时：没在草稿里给出的凭据用已保存的
	Config  map[string]string `json:"config,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	Send    bool              `json:"send,omitempty"`
}

func (a *api) testDraft(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var in draftIn
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	d, ok := a.call.Decl(in.Type)
	if !ok {
		return domain.NewError(domain.CodeInvalidRequest, "unknown connection type "+in.Type, "choose a type from GET /connections/types")
	}
	if in.ID != "" {
		if err := a.st.View(r.Context(), func(tx *store.Tx) error {
			c, err := a.cs.Get(tx, in.ID)
			if err == nil && c.Type != in.Type {
				err = domain.NewError(domain.CodeInvalidRequest, "the connection has another type", "test with its own type")
			}
			return err
		}); err != nil {
			return err
		}
	}
	cfg, err := d.Normalize(in.Config, func(string) bool { return true }) // 缺凭据由调用器报 credentials_missing
	if err != nil {
		return core.WriteJSON(w, http.StatusOK, testResult{Problem: conn.ProblemConfigInvalid, Detail: err.Error()})
	}
	err = a.tests.Probe(r.Context(), conn.Target{ID: in.ID, Type: in.Type, Settings: cfg}, in.Secrets, in.Send, false)
	return core.WriteJSON(w, http.StatusOK, result(err))
}

func (a *api) testSaved(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var c kconn.Connection
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { c, err = a.cs.Get(tx, r.PathValue("id")); return }); err != nil {
		return err
	}
	err := a.tests.Probe(r.Context(), kconn.Target(c), nil, true, true)
	return core.WriteJSON(w, http.StatusOK, result(err))
}
