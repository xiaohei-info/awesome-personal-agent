// Package retention 按保留期清理运行数据（solution-design §6.15、§16.2）：
// Ingest 请求体过期只留元数据（默认 30 天），运行目录过期整个删除（默认 14 天）。
// 判断记录过保留期（默认 180 天）整条删除，命令 Event 里的结论留着，`apa why` 只显示结论（§6.16）。
// 账本事实、Event 和 Run 记录不清理；在途（queued、running）运行的目录不动。
// 本包只删文件系统里的运行目录；Ingest 的清理由 webhook 模块自己的命令执行，不直写其数据。
package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// IngestExpirer 清掉超过保留期的 Ingest 请求体，只留元数据（*webhook.Service 满足它）。
type IngestExpirer interface {
	Expire(ctx context.Context) error
}

// Options 配置清理；Store、Ingests、Layout 必填。
type Options struct {
	Store  *store.Store
	Ingest IngestExpirer
	Layout config.Layout
	// RunRetention 是运行目录保留期，默认 14 天（config.Default）。
	RunRetention time.Duration
	// JudgmentRetention 是判断记录保留期，默认 180 天（config.Default）。
	JudgmentRetention time.Duration
	// KeepJudgments 为真时不清理判断记录（配置 judgment_retention_days: 0）。
	KeepJudgments bool
	Now           func() time.Time
}

// Service 执行清理；Sweep 幂等，可反复调用。
type Service struct{ o Options }

func New(o Options) (*Service, error) {
	if o.Store == nil || o.Ingest == nil || o.Layout.Runs == "" {
		return nil, errors.New("retention: Store, Ingest and Layout are required")
	}
	if o.RunRetention <= 0 {
		o.RunRetention = config.Default().RunRetention
	}
	if o.JudgmentRetention <= 0 {
		o.JudgmentRetention = config.Default().JudgmentRetention
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}, nil
}

// Sweep 先清 Ingest 请求体，再清运行目录和判断记录；三者互不影响，错误合并返回。
func (s *Service) Sweep(ctx context.Context) error {
	return errors.Join(s.o.Ingest.Expire(ctx), s.ExpireRunDirs(ctx), s.ExpireJudgments(ctx))
}

// ExpireJudgments 删除早于保留期的判断记录，返回前不动 Event 与事项。
func (s *Service) ExpireJudgments(ctx context.Context) error {
	if s.o.KeepJudgments {
		return nil
	}
	cut := s.o.Now().Add(-s.o.JudgmentRetention)
	if err := s.o.Store.Update(ctx, func(tx *store.Tx) error { _, err := record.DeleteBefore(tx, cut); return err }); err != nil {
		return fmt.Errorf("retention: expire judgments: %w", err)
	}
	return nil
}

// ExpireRunDirs 删除已结束超过保留期的运行目录。只看终态 Run 的结束时间：
// 排队或运行中的 Run 无论多久都不动；Run 记录本身留在账本。
func (s *Service) ExpireRunDirs(ctx context.Context) error {
	cut := s.o.Now().Add(-s.o.RunRetention)
	var recs []proc.Record
	if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { recs, err = proc.List(tx); return }); err != nil {
		return fmt.Errorf("retention: list runs: %w", err)
	}
	var errs []error
	for _, r := range recs {
		if !r.Run.State.Terminal() || r.Run.FinishedAt == nil || !r.Run.FinishedAt.Before(cut) {
			continue
		}
		// 运行 id 来自账本，仍要确认它只是 runs/ 下的一级目录名，防止 id 带路径逃出数据目录。
		if r.Run.ID == "" || r.Run.ID != filepath.Base(r.Run.ID) || r.Run.ID == "." || r.Run.ID == ".." {
			errs = append(errs, fmt.Errorf("retention: run id %q is not a plain directory name", r.Run.ID))
			continue
		}
		if err := os.RemoveAll(s.o.Layout.RunDir(r.Run.ID)); err != nil {
			errs = append(errs, fmt.Errorf("retention: remove run dir %s: %w", r.Run.ID, err))
		}
	}
	return errors.Join(errs...)
}
