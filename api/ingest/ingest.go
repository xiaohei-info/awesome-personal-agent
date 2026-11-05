// Package ingest 是 webhook 接收路由（设计 §9.5，附录 B.2）：POST /ingest/{source_id}，不带 /v1 前缀。
//
// 路由里没有业务规则：校验（401，不存请求体）、落盘后才回 2xx、平台握手、按投递 id 幂等、频率限制（429）
// 都由 kernel/webhook 判定，本层只把 HTTP 请求转成 webhook.Request，再把 webhook.Response 原样写回。
// 接收密钥不是三级 token：路由用 core.AuthNone，凭据由校验方案自己从请求头取，
// 所以拿它当 Bearer 访问任何 /v1 路由都在骨干认证处被拒绝。
package ingest

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
)

// Routes 返回 webhook 接收路由分组，交给 core.Deps.Groups。maxBody 是请求体上限（字节），
// 须与 webhook.Options.MaxBody 一致；不大于 0 时用骨干默认值。
func Routes(svc *webhook.Service, maxBody int64) core.RouteGroup {
	if maxBody <= 0 {
		maxBody = core.DefaultMaxBody
	}
	return func(r *core.Router) {
		r.HandleRoot(core.Route{Method: "POST", Path: "/ingest/{source_id}", Auth: core.AuthNone, MaxBody: maxBody,
			Handler: func(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					var tooBig *http.MaxBytesError
					if errors.As(err, &tooBig) {
						return core.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
					}
					return core.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "request body could not be read"})
				}
				h := make(map[string]string, len(r.Header))
				for k := range r.Header {
					h[k] = r.Header.Get(k)
				}
				// 落盘与回包不随客户端断开而取消：已验签的请求要么完整落盘，要么明确失败。
				res := svc.Receive(context.WithoutCancel(r.Context()), webhook.Request{SourceID: r.PathValue("source_id"), Header: h, Body: body})
				if res.ContentType != "" {
					w.Header().Set("Content-Type", res.ContentType)
				}
				w.WriteHeader(res.Status)
				_, err = w.Write(res.Body)
				return err
			}})
	}
}
