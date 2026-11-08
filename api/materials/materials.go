// Package materials 是材料分组的 HTTP 路由（附录 B.2、设计 §12.4）：
//
//	GET/POST /materials          GET /materials/{id}          GET /materials/search
//	GET/POST /materials/{id}/revisions     POST /materials/{id}/revert
//	POST /revisions/{id}/confirm|reject
//	GET /materials/{id}/content  POST /materials/{id}/changes   （托管型，需要 hosted.Service）
//	PUT/GET /blobs/{hash}
//
// 路由里没有业务规则：写入全部映射到内核命令（material.commit/add、revision.confirm|reject、material.revert、blob.put|get），
// 授权（回退与规范生效只认 Web、delegate 代答须附本人原话）、写入策略、基线冲突、幂等都由命令管线判定。
// 本层只做 HTTP 到命令请求的搬运：路径里的 id 注入请求体，Idempotency-Key 头 → 幂等键，X-APA-Evidence 头 → 本人原话。
// 唯一的路由级判定是 /content 只给 Web 登录——托管型读取走 hosted.Service，不经命令管线，管线无从授权。
package materials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	km "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// IdempotencyHeader 携带幂等键；同键同输入重放首次结果，同键异输入返回 conflict。
	IdempotencyHeader = "Idempotency-Key"
	// MaxBlob 是单个附件的上限。
	// shortcut: 整块读入内存再 base64 进命令输入，所以设上限；要传更大的文件时改成流式落盘。
	MaxBlob int64 = 32 << 20
)

// Routes 返回材料路由分组，交给 core.Deps.Groups。x 须已登记 materials、hosted、blobs 的命令；
// hs 是托管型读取与修改服务（生产装配必须给；nil 时不挂 /content 与 /changes）。
func Routes(st *store.Store, x *command.Executor, hs *hosted.Service) core.RouteGroup {
	h := &handlers{st: st, x: x, hs: hs}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/materials", Handler: h.list})
		r.Handle(core.Route{Method: "POST", Path: "/materials", Handler: h.create})
		r.Handle(core.Route{Method: "GET", Path: "/materials/search", Handler: h.search})
		r.Handle(core.Route{Method: "GET", Path: "/materials/{id}", Handler: h.get})
		r.Handle(core.Route{Method: "GET", Path: "/materials/{id}/revisions", Handler: h.revisions})
		r.Handle(core.Route{Method: "POST", Path: "/materials/{id}/revisions", Handler: h.write("material.commit", http.StatusCreated)})
		r.Handle(core.Route{Method: "POST", Path: "/materials/{id}/revert", Handler: h.write("material.revert", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/revisions/{id}/confirm", Handler: h.write("revision.confirm", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/revisions/{id}/reject", Handler: h.write("revision.reject", http.StatusOK)})
		if hs != nil { // 托管型读取与修改依赖读取器服务；没有就不挂这两条，而不是挂一个必然 panic 的处理器
			r.Handle(core.Route{Method: "GET", Path: "/materials/{id}/content", Handler: h.content})
			r.Handle(core.Route{Method: "POST", Path: "/materials/{id}/changes", Handler: h.changes})
		}
		r.Handle(core.Route{Method: "PUT", Path: "/blobs/{hash}", MaxBody: MaxBlob, Handler: h.putBlob})
		r.Handle(core.Route{Method: "GET", Path: "/blobs/{hash}", Handler: h.getBlob})
	}
}

type handlers struct {
	st *store.Store
	x  *command.Executor
	hs *hosted.Service
}

// readable：读材料对 observer 及以上开放；ext 与 webhook 凭据另有用途，不能读账本，只有导出 ext 例外（§10.3）。
func readable(p *core.Principal) error {
	if auth.IsExportExt(p.Caller.Kind, p.Caller.Actor.Principal) {
		return nil
	}
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read materials", "use an owner or agent credential")
	}
	return nil
}

func invalid(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

// object 读出请求体里的 JSON 对象。
func object(r *http.Request) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := core.DecodeJSON(r, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, invalid("request body must be a JSON object", "send a single well-formed JSON object")
	}
	return m, nil
}

// run 以请求的凭据执行一条内核命令。evidence 是请求元数据，不是命令输入（命令对未知字段严格拒绝），所以从体里取出。
func (h *handlers) run(r *http.Request, p *core.Principal, cmd string, in map[string]json.RawMessage) (command.Result, error) {
	req := command.Request{Identity: p.Identity, Command: cmd, IdemKey: r.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL}
	if raw, ok := in["evidence"]; ok {
		delete(in, "evidence")
		if req.Evidence == "" {
			if err := json.Unmarshal(raw, &req.Evidence); err != nil {
				return command.Result{}, invalid("evidence must be a string", "send the owner's words as a string")
			}
		}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return command.Result{}, err
	}
	req.Input = b
	return h.x.Do(context.WithoutCancel(r.Context()), req)
}

func reply(w http.ResponseWriter, status int, res command.Result) error {
	if res.Replayed {
		status = http.StatusOK
	}
	return core.WriteJSON(w, status, json.RawMessage(res.Value))
}

// write 把请求体交给命令，路径里的 id 注入体的 id（体里不许再写，避免两处不一致）。
func (h *handlers) write(cmd string, status int) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		in, err := object(r)
		if err != nil {
			return err
		}
		if _, dup := in["id"]; dup {
			return invalid("field id comes from the URL and cannot be in the body", "remove it from the body")
		}
		in["id"], _ = json.Marshal(r.PathValue("id"))
		res, err := h.run(r, p, cmd, in)
		if err != nil {
			return err
		}
		return reply(w, status, res)
	}
}

// create：带 repo 或 path 是登记托管型（material.add），否则是新建自有型的首条修订（material.commit）。
func (h *handlers) create(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in, err := object(r)
	if err != nil {
		return err
	}
	cmd := "material.commit"
	if _, ok := in["repo"]; ok {
		cmd = "material.add"
	} else if _, ok := in["path"]; ok {
		cmd = "material.add"
	}
	if _, has := in["id"]; has {
		return invalid("id is assigned by the server; to revise a material use /materials/{id}/revisions", "remove id")
	}
	res, err := h.run(r, p, cmd, in)
	if err != nil {
		return err
	}
	return reply(w, http.StatusCreated, res)
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	var ms []km.Material
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) { ms, err = km.List(tx, r.URL.Query().Get("project")); return }); err != nil {
		return err
	}
	if ms == nil {
		ms = []km.Material{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"materials": ms})
}

// get 返回登记与关联事项（relations），加上自有型的修订（默认当前修订，?rev= 指定）或托管型的登记详情。
func (h *handlers) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	id, rev := r.PathValue("id"), r.URL.Query().Get("rev")
	out := map[string]any{}
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		m, err := km.Get(tx, id)
		if err != nil {
			return err
		}
		out["material"] = m
		// 事项与材料的类型化关系（item.relate 以材料为终点的边），材料一侧同样可见。
		if out["relations"], err = items.Relations(tx, id); err != nil {
			return err
		}
		switch {
		case m.Location != "":
			e, err := hosted.Get(tx, id)
			out["hosted"] = e
			return err
		case m.CurrentRev == "" && rev == "":
			out["revision"] = nil // 只有待确认的修订
			return nil
		}
		_, v, err := km.Show(tx, id, rev)
		out["revision"] = v
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

func (h *handlers) revisions(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	var vs []km.Revision
	if err := h.st.View(r.Context(), func(tx *store.Tx) error {
		if _, err := km.Get(tx, r.PathValue("id")); err != nil {
			return err
		}
		var err error
		vs, err = km.History(tx, r.PathValue("id"))
		return err
	}); err != nil {
		return err
	}
	if vs == nil {
		vs = []km.Revision{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"revisions": vs})
}

func (h *handlers) search(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	v := r.URL.Query()
	q := search.Query{Text: v.Get("q"), Project: v.Get("project"), Kind: v.Get("kind"), Tags: v["tag"], Item: v.Get("item")}
	for name, dst := range map[string]*time.Time{"after": &q.After, "before": &q.Before} {
		if s := v.Get(name); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return invalid(name+" must be an RFC 3339 time", "fix "+name+" and retry")
			}
			*dst = t
		}
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return invalid("limit must be a positive integer", "fix limit and retry")
		}
		q.Limit = n
	}
	hits, err := search.Search(r.Context(), h.st, q)
	if err != nil {
		return err
	}
	if hits == nil {
		hits = []search.Hit{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

// content 把 hosted.View 渲染成附录 B.2 的三态。读取失败是降级结果（unavailable），不是错误。
func (h *handlers) content(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if !p.Web {
		return domain.NewError(domain.CodeWebOnly, "hosted material content can only be read in the Web UI", "ask the owner to open it in Web")
	}
	v, err := h.hs.Content(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	out := map[string]any{"repo": v.Entry.Repo, "path": v.Entry.Path}
	switch v.Status {
	case hosted.StatusReading:
		out = map[string]any{"state": "reading"}
	case hosted.StatusReady:
		out["state"], out["content"], out["commit"] = "ready", v.Content, v.Commit
	default:
		out["state"], out["reason"] = "unavailable", v.Reason
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

func (h *handlers) changes(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in hosted.ChangeIn
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.ID != "" {
		return invalid("field id comes from the URL and cannot be in the body", "remove it from the body")
	}
	in.ID, in.IdemKey, in.Evidence = r.PathValue("id"), r.Header.Get(IdempotencyHeader), p.Evidence
	out, err := h.hs.RequestChange(context.WithoutCancel(r.Context()), p.Identity, in)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusCreated, map[string]any{"item": out.Child, "parent": out.Parent})
}

func (h *handlers) putBlob(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return invalid("request body is too large or unreadable", "send the file within the size limit")
	}
	sum := sha256.Sum256(data)
	if want := strings.ToLower(r.PathValue("hash")); hex.EncodeToString(sum[:]) != want {
		return invalid("the body does not hash to the sha256 in the URL", "put the file at /blobs/<sha256 of its content>")
	}
	in, _ := json.Marshal(map[string]any{"content": data})
	res, err := h.x.Do(context.WithoutCancel(r.Context()), command.Request{Identity: p.Identity, Command: "blob.put", Input: in, IdemKey: r.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL})
	if err != nil {
		return err
	}
	var put blobs.PutResult
	if err := json.Unmarshal(res.Value, &put); err != nil {
		return err
	}
	status := http.StatusOK
	if put.Created {
		status = http.StatusCreated
	}
	return core.WriteJSON(w, status, put)
}

func (h *handlers) getBlob(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	in, _ := json.Marshal(map[string]string{"hash": r.PathValue("hash")})
	res, err := h.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: "blob.get", Input: in})
	if err != nil {
		return err
	}
	var got blobs.GetResult
	if err := json.Unmarshal(res.Value, &got); err != nil {
		return err
	}
	// 附件是不可信的任意字节：不让浏览器就地渲染（nosniff 由骨干统一加）。
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Content-Disposition", "attachment")
	hd.Set("Content-Length", strconv.Itoa(len(got.Content)))
	_, err = w.Write(got.Content)
	return err
}
