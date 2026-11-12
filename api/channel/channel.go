// Package channel 是回答回流与一次性表单的 HTTP 路由（附录 B.2，设计 §10.3）：
//
//	POST /deliveries/{id}/answer   渠道 ext 转达回答（渠道 ext 密钥）
//	GET  /forms/{令牌}              读表单：只给问题与选项
//	POST /forms/{令牌}              作答（令牌本身就是凭据，一次性）
//
// 路由里没有业务规则：回答人白名单、ref 一致、版本作废、web_only、初筛未返回、令牌一次性都由
// kernel/channel 的命令判定，被拒绝的错误由骨干渲染成统一错误体。本层只做 HTTP 到命令请求的搬运：
// 路径里的 id 或令牌注入输入（体里不许再写）。表单路由用 core.AuthNone，令牌只换到这一个决策的一次作答，
// 不授予任何账本读写权，所以 /forms 下没有别的操作，拿令牌当 Bearer 访问 /v1 也会在骨干处被拒绝。
package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// IdempotencyHeader 携带幂等键；同键同输入重放首次结果。
const IdempotencyHeader = "Idempotency-Key"

// formIdentity 是令牌兑现时的调用身份：令牌已由 kernel/channel 校验，身份只用于记账（resolved_via 取发放时的渠道）。
var formIdentity = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "form", Via: domain.ViaExt}, Level: authz.LevelWorker}

// Routes 返回渠道路由分组，交给 core.Deps.Groups。x 须已经 channel.Register。now 为 nil 用系统时钟，须与 x 同一时钟。
func Routes(x *command.Executor, st *store.Store, now func() time.Time) core.RouteGroup {
	if now == nil {
		now = clock.Now
	}
	h := &handlers{x: x, st: st, now: now}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "POST", Path: "/deliveries/{id}/answer", Handler: h.answer})
		// 表单是人点开的链接，不带 /v1（投递里的 form_url 是 /forms/<令牌>）。
		r.HandleRoot(core.Route{Method: "GET", Path: "/forms/{token}", Auth: core.AuthNone, Page: true, Handler: h.formShow})
		r.HandleRoot(core.Route{Method: "POST", Path: "/forms/{token}", Auth: core.AuthNone, Handler: h.formAnswer})
	}
}

type handlers struct {
	x   *command.Executor
	st  *store.Store
	now func() time.Time
}

// inject 解析 JSON 对象体，拒绝体里重复写路径参数，再把路径参数按命令输入的字段名写进去。
func inject(r *http.Request, field, value string) (json.RawMessage, error) {
	var body map[string]json.RawMessage
	if err := core.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	if body == nil { // JSON null
		return nil, domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
	}
	for k := range body {
		if strings.EqualFold(k, field) {
			return nil, domain.NewError(domain.CodeInvalidRequest, "field "+k+" comes from the URL and cannot be in the body", "remove it from the body")
		}
	}
	body[field], _ = json.Marshal(value)
	return json.Marshal(body)
}

func (h *handlers) answer(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in, err := inject(r, "delivery", r.PathValue("id"))
	if err != nil {
		return err
	}
	res, err := h.x.Do(context.WithoutCancel(r.Context()), command.Request{Identity: p.Identity, Command: channel.CmdAnswer,
		Input: in, IdemKey: r.Header.Get(IdempotencyHeader)})
	if err != nil {
		return err
	}
	return write(w, res)
}

func (h *handlers) formAnswer(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	in, err := inject(r, "token", r.PathValue("token"))
	if err != nil {
		return err
	}
	res, err := h.x.Do(context.WithoutCancel(r.Context()), command.Request{Identity: formIdentity, Command: channel.CmdFormAnswer, Input: in})
	if err != nil {
		return err
	}
	return write(w, res)
}

// write 回 {"id": 事项, "replayed": 是否幂等重放}；令牌在 URL 里，不许被缓存或经 Referer 外泄。
func write(w http.ResponseWriter, res command.Result) error {
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &out); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return core.WriteJSON(w, http.StatusOK, map[string]any{"id": out.ID, "replayed": res.Replayed})
}

type formOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// formShow 只读：不兑现令牌。只回问题与选项，不回事项正文或其他账本内容。
func (h *handlers) formShow(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var out struct {
		Question string       `json:"question"`
		Options  []formOption `json:"options"`
		FreeText bool         `json:"free_text"`
	}
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		f, err := channel.PeekForm(tx, r.PathValue("token"), h.now())
		if err != nil {
			return err
		}
		it, err := items.Get(tx, f.Item)
		if err != nil {
			return err
		}
		if ver, err := command.Version(tx, f.Item); err != nil {
			return err
		} else if ver != f.Version || it.Ask == nil || it.Ask.Resolution != nil {
			return domain.NewError(domain.CodeConflict, "this form link is outdated; the item has changed", "use the latest message or answer in the Web UI")
		}
		out.Question, out.FreeText = it.Ask.Question, it.Ask.FreeText
		out.Options = make([]formOption, 0, len(it.Ask.Options))
		for _, o := range it.Ask.Options {
			out.Options = append(out.Options, formOption{o.ID, o.Label})
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	return core.WriteJSON(w, http.StatusOK, out)
}
