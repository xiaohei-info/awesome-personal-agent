package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 来源原文的保留期清除（§6.15）：来源设了 content_retention_days 才清（默认不清理）；
// 落盘超过天数、已处置、且不挂在任何未关闭事项上的事件，原文各部分与预览清掉，事件行留元信息并标 purged；
// 附件清单里已取回的 blob 在没有别的引用时一并删除。每轮每个来源清掉了东西就记一条内部审计事件（条数与字节数），不逐条记。

// CmdPurgeContent 是清除一个来源到期原文的内部命令；EvContentPurged 是它的审计事件（不在对外事件类型表里）。
const (
	CmdPurgeContent = "retention.purge_content"
	EvContentPurged = "source.content_purged"
)

// system 是维护循环的身份；命令的对象记为它自己，使这次写入属于"改自己的对象"（同来源调度）。
var system = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "kernel:retention", Via: domain.ViaClock}, Level: authz.LevelWorker}

type contentIn struct {
	SourceID string `json:"source_id"`
}

// ContentResult 是一轮清除的结果。
type ContentResult struct {
	SourceID     string   `json:"source_id"`
	Events       int      `json:"events"`
	Bytes        int64    `json:"bytes"`
	BlobsRemoved []string `json:"blobs_removed,omitempty"`
}

// registerContent 登记 retention.purge_content；blobDir 是 <APA_HOME>/blobs。
func registerContent(x *command.Executor, blobDir string) error {
	return x.Register(command.Spec{
		Name: CmdPurgeContent, Action: authz.ActionUpdateOwn,
		Object: func(_ *store.Tx, raw json.RawMessage) (authz.Object, error) {
			var in contentIn
			if err := json.Unmarshal(raw, &in); err != nil {
				return authz.Object{}, domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
			}
			return authz.Object{ID: in.SourceID, Assignee: system.Actor.Principal}, nil
		},
		Apply: func(c *command.Ctx) (any, error) { return applyContent(c, blobDir) },
	})
}

func applyContent(c *command.Ctx, blobDir string) (any, error) {
	var in contentIn
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return nil, domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
	}
	src, err := registry.Get(c.Tx, in.SourceID)
	if err != nil {
		return nil, err
	}
	res := ContentResult{SourceID: src.ID}
	if src.ContentRetentionDays == nil { // 没设天数即不清理
		return res, nil
	}
	cut := c.Now().Add(-time.Duration(*src.ContentRetentionDays) * 24 * time.Hour)
	due, err := runner.ExpiredContent(c.Tx, src.ID, cut)
	if err != nil {
		return nil, err
	}
	var fetched []string
	for _, e := range due {
		open, err := anyOpen(c.Tx, e.Items)
		if err != nil {
			return nil, err
		}
		if open {
			continue
		}
		n, hs, err := runner.PurgeContent(c.Tx, e.ID)
		if err != nil {
			return nil, err
		}
		res.Events++
		res.Bytes += n
		fetched = append(fetched, hs...)
	}
	if res.Events == 0 {
		return res, nil
	}
	slices.Sort(fetched)
	var drop []string
	for _, h := range slices.Compact(fetched) {
		used, err := materials.BlobReferenced(c.Tx, blobs.Ref(h))
		if err == nil && !used {
			used, err = runner.BlobReferenced(c.Tx, h)
		}
		if err != nil {
			return nil, err
		}
		if !used {
			drop = append(drop, h)
		}
	}
	res.BlobsRemoved = drop
	payload, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvContentPurged, Object: src.ID, Payload: payload}); err != nil {
		return nil, err
	}
	// 文件删除放在最后：之前任何一步失败都回滚事务且不丢 blob；删文件后提交失败时，下一轮重做同一批即可补完（blob 本就无引用）。
	for _, h := range drop {
		if err := blobs.Remove(blobDir, h); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "blob storage failed", "check the data directory and retry")
		}
	}
	return res, nil
}

// anyOpen 报告其中是否有未关闭（非 done、dropped）的事项；已不存在的事项视为已关闭。
func anyOpen(tx *store.Tx, ids []string) (bool, error) {
	for _, id := range ids {
		it, err := items.Get(tx, id)
		var de *domain.Error
		switch {
		case errors.As(err, &de) && de.Code == domain.CodeNotFound:
		case err != nil:
			return false, err
		case !it.Phase.Terminal():
			return true, nil
		}
	}
	return false, nil
}

// ExpireContent 对每个设了保留天数的来源清一轮到期原文；某个来源失败不影响其余，错误合并返回。
func (s *Service) ExpireContent(ctx context.Context) error {
	var srcs []domain.Source
	if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { srcs, err = registry.List(tx); return }); err != nil {
		return fmt.Errorf("retention: list sources: %w", err)
	}
	var errs []error
	for _, src := range srcs {
		if src.ContentRetentionDays == nil {
			continue
		}
		body, _ := json.Marshal(contentIn{SourceID: src.ID})
		if _, err := s.o.Exec.Do(ctx, command.Request{Identity: system, Command: CmdPurgeContent, Input: body}); err != nil {
			errs = append(errs, fmt.Errorf("retention: purge content of %s: %w", src.ID, err))
		}
	}
	return errors.Join(errs...)
}
