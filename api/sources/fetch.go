package sources

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 附件取回（设计 §7.6、附录 D.8）：POST /source-events/{id}/attachments/{n}/fetch。
// 顺序：预检（授权与校验，没有权限的调用者触发不了来源程序）→ 运行来源程序的 fetch 模式 → 附件存为内容寻址附件 →
// source.fetch_attachment 在一个事务里把 blob 记进清单并写审计事件。取回不产生来源事件、不推进游标；任何一步失败，清单不变。

type fetchIn struct {
	ID   string `json:"id"`
	N    int    `json:"n"`
	Hash string `json:"hash,omitempty"` // 提交一步才有：已存下的内容的 sha256
}

// fetchTarget 在事务里解出要取回的附件：事件与来源存在、来源的程序支持取回、清单有第 n 项且带取回句柄。
func (a *api) fetchTarget(tx *store.Tx, id string, n int) (domain.Source, domain.SourceEvent, runner.AttachmentRef, error) {
	var zero runner.AttachmentRef
	ev, err := runner.Event(tx, id)
	if err != nil {
		return domain.Source{}, ev, zero, err
	}
	src, err := registry.Get(tx, ev.SourceID)
	if err != nil {
		return src, ev, zero, err
	}
	if a.dc.CanFetch == nil || !a.dc.CanFetch(src) {
		return src, ev, zero, domain.NewError(domain.CodeConflict, "the source program does not support fetching attachments", "open the original message instead")
	}
	list, err := runner.EventAttachments(tx, id)
	if err != nil {
		return src, ev, zero, err
	}
	if n < 0 || n >= len(list) {
		return src, ev, zero, domain.NewError(domain.CodeNotFound, "source event "+id+" has no attachment "+strconv.Itoa(n), "read the content manifest for the attachments it has")
	}
	if list[n].Ref == "" {
		return src, ev, list[n], domain.NewError(domain.CodeConflict, "the attachment has no fetch handle", "open the original message instead")
	}
	return src, ev, list[n], nil
}

func (a *api) validateFetch(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
	var p fetchIn
	if err := json.Unmarshal(in, &p); err != nil {
		return invalid("bad input: "+err.Error(), "fix the request and retry")
	}
	_, _, _, err := a.fetchTarget(tx, p.ID, p.N)
	return err
}

func (a *api) applyFetch(c *command.Ctx) (any, error) {
	var p fetchIn
	if err := json.Unmarshal(c.Input, &p); err != nil {
		return nil, invalid("bad input: "+err.Error(), "fix the request and retry")
	}
	if p.Hash == "" {
		return nil, invalid("hash is required to record a fetched attachment", "fetch through POST /source-events/{id}/attachments/{n}/fetch")
	}
	raw, err := runner.SetAttachmentBlob(c.Tx, p.ID, p.N, p.Hash)
	if err != nil {
		return nil, err
	}
	var item runner.AttachmentRef
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"event": p.ID, "n": p.N, "name": item.Name, "blob": item.Blob, "size": item.Size})
	if _, err := c.Emit(domain.Event{Type: EvAttachmentFetched, Object: p.ID, Payload: payload}); err != nil {
		return nil, err
	}
	return raw, nil
}

func (a *api) fetchAttachment(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 {
		return invalid("n must be a non-negative integer", "use the attachment's position in the manifest, counting from 0")
	}
	in := fetchIn{ID: r.PathValue("id"), N: n}
	if _, err := a.runInput(r, p, CmdFetchAttachment, in, true); err != nil {
		return err
	}
	var (
		src domain.Source
		ev  domain.SourceEvent
		att runner.AttachmentRef
	)
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { src, ev, att, err = a.fetchTarget(tx, in.ID, n); return }); err != nil {
		return err
	}
	if a.dc.FetchAttachment == nil || a.dc.BlobsDir == "" {
		return domain.NewError(domain.CodeConflict, "attachment fetching is not available", "open the original message instead")
	}
	content, err := a.dc.FetchAttachment(r.Context(), src, ev, n, att)
	if err != nil {
		return err
	}
	put, err := blobs.Put(a.dc.BlobsDir, content)
	if err != nil {
		return err
	}
	in.Hash = put.Hash
	res, err := a.runInput(r, p, CmdFetchAttachment, in, false)
	if err != nil {
		if put.Created { // 命令没提交（事件在途中被删、授权变了）：不留没人引用的附件
			a.dropUnreferenced(r, put.Hash)
		}
		return err
	}
	return core.WriteJSON(w, http.StatusOK, json.RawMessage(res.Value))
}

// dropUnreferenced 删掉没有任何来源事件清单引用的 blob 文件；尽力而为，删不掉只是留一个孤立文件。
func (a *api) dropUnreferenced(r *http.Request, hash string) {
	_ = a.st.View(r.Context(), func(tx *store.Tx) error {
		if ref, err := runner.BlobReferenced(tx, hash); err == nil && !ref {
			_ = blobs.Remove(a.dc.BlobsDir, hash)
		}
		return nil
	})
}
