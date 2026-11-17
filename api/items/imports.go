// 导入的 HTTP 路由（附录 B.2，设计 §7.1、§14.5）：
//
//	POST /import?source=<来源名>[&batch=<批次>][&dry_run=1]    GET /imports
//
// 路由只做传输：读出 JSONL 请求体（不超过 500 行、8 MiB，超限 413）、把授权头与幂等键交给 kernel/imports；
// 授权（本人在 Web 不需要授权单；delegate 只凭导入专用授权单；运行凭据 403）、逐行校验、幂等与批次都在内核。
package items

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kimports "github.com/xiaohei-info/awesome-personal-agent/kernel/imports"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ImportContentType 是导入请求体的媒体类型。Idempotency-Key 头沿用 IdempotencyHeader：同一键重放得到同一批次与同一结果。
const ImportContentType = "application/x-ndjson"

type importHandlers struct {
	svc *kimports.Service
	st  *store.Store
}

// ImportRoutes 返回导入路由分组，交给 core.Deps.Groups。svc 须已由 kimports.Register 建出。
func ImportRoutes(st *store.Store, svc *kimports.Service) core.RouteGroup {
	h := &importHandlers{svc: svc, st: st}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "POST", Path: "/import", MaxBody: kimports.MaxBytes, Handler: h.post})
		r.Handle(core.Route{Method: "GET", Path: "/imports", Handler: h.list})
	}
}

func tooLarge(w http.ResponseWriter, why string) error {
	return core.WriteJSON(w, http.StatusRequestEntityTooLarge, domain.NewError(domain.CodeInvalidRequest, why, "split the file into requests of at most 500 lines and 8 MiB"))
}

func (h *importHandlers) post(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != ImportContentType {
		return domain.NewError(domain.CodeInvalidRequest, "Content-Type must be "+ImportContentType, "send one JSON record per line")
	}
	q := r.URL.Query()
	data, err := io.ReadAll(r.Body)
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		return tooLarge(w, "the request body is larger than 8 MiB")
	} else if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "the request body could not be read", "retry")
	}
	var lines []kimports.Line
	for i, raw := range bytes.Split(data, []byte("\n")) {
		if raw = bytes.TrimSpace(raw); len(raw) > 0 {
			lines = append(lines, kimports.Line{N: i + 1, Text: raw})
		}
	}
	if len(lines) > kimports.MaxRecords {
		return tooLarge(w, "the request has more than 500 lines")
	}
	res, err := h.svc.Run(context.WithoutCancel(r.Context()), kimports.Params{Identity: p.Identity, Source: q.Get("source"), Batch: q.Get("batch"),
		DryRun: truthy(q.Get("dry_run")), Lines: lines, IdemKey: r.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res)
}

func truthy(v string) bool { return v == "1" || strings.EqualFold(v, "true") }

func (h *importHandlers) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read import batches", "use an owner or agent credential")
	}
	var bs []kimports.Batch
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) { bs, err = kimports.Batches(tx); return }); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"batches": bs})
}
