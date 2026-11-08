// Package delegation 是委派与声明分组的 HTTP 路由（附录 B.2，设计 §7.5、§14.3）：
// GET /executors、POST /delegations、POST /items/{id}/report、POST /declarations。
//
// 路由里没有业务规则：全部映射到内核命令（executor.list、delegate.create、delegate.report、executor.declare），
// 授权（项目允许的执行者 21、运行身份范围 4、声明须 Web 确认）、校验、乐观版本与幂等都由命令管线判定。
// 本层只做 HTTP 到命令请求的搬运：
//   - 路径里的 id 注入回执请求体（体里不许再写 id）；
//   - Idempotency-Key 头 → 幂等键；If-Match 头 → 期望版本；
//   - GET /executors 的 ?project= → executor.list 的 project。
//
// /messages/{id}/handback 不在本分组（cli-followmsg）。
package delegation

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const (
	// IdempotencyHeader 携带幂等键；同键同输入重放首次结果，同键异输入返回 conflict。
	IdempotencyHeader = "Idempotency-Key"
	// IfMatchHeader 携带期望的对象版本（整数，可带引号）；不符返回 conflict。
	IfMatchHeader = "If-Match"
)

// Routes 返回委派路由分组，交给 core.Deps.Groups。x 须已经登记 executors、delegation 与 items 命令。
func Routes(x *command.Executor) core.RouteGroup {
	h := &handlers{x: x}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/executors", Handler: h.executors})
		r.Handle(core.Route{Method: "POST", Path: "/delegations", Handler: h.write("delegate.create", false, http.StatusCreated)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/report", Handler: h.write("delegate.report", true, http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/declarations", Handler: h.write("executor.declare", false, http.StatusAccepted)})
	}
}

type handlers struct{ x *command.Executor }

func (h *handlers) executors(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in := json.RawMessage(`{}`)
	if proj := r.URL.Query().Get("project"); proj != "" {
		in, _ = json.Marshal(map[string]string{"project": proj})
	}
	res, err := h.x.Do(context.WithoutCancel(r.Context()), command.Request{Identity: p.Identity, Command: "executor.list", Input: in})
	if err != nil {
		return err
	}
	return writeRaw(w, http.StatusOK, res.Value)
}

func writeRaw(w http.ResponseWriter, status int, v json.RawMessage) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err := w.Write(v)
	return err
}

// write 把请求体原样交给命令。withID 为真时把路径 id 写入体的 id（体里自带 id 直接拒绝）。
func (h *handlers) write(cmd string, withID bool, status int) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		var body map[string]json.RawMessage
		if err := core.DecodeJSON(r, &body); err != nil {
			return err
		}
		if body == nil { // JSON null
			return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
		}
		if withID {
			for k := range body {
				if strings.EqualFold(k, "id") {
					return domain.NewError(domain.CodeInvalidRequest, "field "+k+" comes from the URL and cannot be in the body", "remove it from the body")
				}
			}
			body["id"], _ = json.Marshal(r.PathValue("id"))
		}
		in, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req := command.Request{Identity: p.Identity, Command: cmd, Input: in, IdemKey: r.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL}
		if v := r.Header.Get(IfMatchHeader); v != "" {
			n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
			if err != nil || n < 0 {
				return domain.NewError(domain.CodeInvalidRequest, IfMatchHeader+" must be an object version number", "send the version from the last read")
			}
			req.ExpectVersion = &n
		}
		res, err := h.x.Do(context.WithoutCancel(r.Context()), req)
		if err != nil {
			return err
		}
		var out map[string]any
		if err := json.Unmarshal(res.Value, &out); err != nil || out == nil {
			out = map[string]any{}
		}
		out["version"], out["replayed"] = res.Version, res.Replayed
		if res.Replayed {
			status = http.StatusOK
		}
		return core.WriteJSON(w, status, out)
	}
}
