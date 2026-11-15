package channel

import (
	"net/http"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// IdentifyRoutes 是"识别我的 ID"的路由（§14.6、附录 B.2）：
//
//	POST /settings/channels/{name}/identify   发起识别，返回一次性口令（只 Web）
//	GET  /settings/channels/{name}/identify   查看识别结果（只 Web）
//	POST /channels/identify                   渠道 ext 上报谁发来了口令（渠道 ext 密钥）
//
// 识别只在内存里；写回答人名单要本人在 Web 另走 PUT /settings/channels/{name}/answerers。
func IdentifyRoutes(id *channel.Identifier) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "POST", Path: "/settings/channels/{name}/identify", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			if err := webOnly(p); err != nil {
				return err
			}
			got, err := id.Start(r.PathValue("name"))
			if err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, map[string]any{"code": got.Code, "expires_at": got.ExpiresAt})
		}})
		r.Handle(core.Route{Method: "GET", Path: "/settings/channels/{name}/identify", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			if err := webOnly(p); err != nil {
				return err
			}
			got, err := id.Status(r.PathValue("name"))
			if err != nil {
				return err
			}
			w.Header().Set("Cache-Control", "no-store")
			return core.WriteJSON(w, http.StatusOK, got)
		}})
		r.Handle(core.Route{Method: "POST", Path: "/channels/identify", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			name, ok := strings.CutPrefix(p.Caller.Actor.Principal, extScope)
			if p.Caller.Kind != auth.Ext || !ok || name == "" {
				return domain.NewError(domain.CodePermissionDenied, "only a channel ext key can report an identification", "use the channel's ext key")
			}
			var in struct {
				Code    string `json:"code"`
				Sender  string `json:"sender"`
				Display string `json:"display"`
			}
			if err := core.DecodeJSON(r, &in); err != nil {
				return err
			}
			if err := id.Report(name, in.Code, in.Sender, in.Display); err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		}})
	}
}

// extScope 与 kernel/channel 一致：渠道 ext 密钥的 Principal 是 "ext:channel.<名>"。
const extScope = "ext:channel."

func webOnly(p *core.Principal) error {
	if p.Web {
		return nil
	}
	return domain.NewError(domain.CodeWebOnly, "channel settings can only be used by the owner in the Web UI", "open Settings › Channels in the Web UI")
}
