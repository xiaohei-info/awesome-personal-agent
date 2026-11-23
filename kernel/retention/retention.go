// Package retention 按保留期清理运行数据（solution-design §6.15、§16.2）：
// Ingest 请求体过期只留元数据（默认 30 天），运行目录过期整个删除（默认 14 天）。
// 判断记录过保留期（默认 180 天）整条删除，命令 Event 里的结论留着，`apa why` 只显示结论（§6.16）。
// 来源原文按来源的 content_retention_days 清除（content.go，默认不清理）。
// 账本事实、Event 和 Run 记录不清理；在途（queued、running）运行的目录不动。
// 本包只删文件系统里的运行目录；Ingest 的清理由 webhook 模块自己的命令执行，不直写其数据。
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// IngestExpirer 清掉超过保留期的 Ingest 请求体，只留元数据（*webhook.Service 满足它）。
type IngestExpirer interface {
	ExpireOlderThan(ctx context.Context, keep time.Duration) (int64, error)
}

// Options 配置清理；Store、Ingests、Layout 必填。
type Options struct {
	Store  *store.Store
	Ingest IngestExpirer
	Layout config.Layout
	// Exec 执行来源原文清除命令（Register 登记的 retention.purge_content）。
	Exec *command.Executor
	// RunRetention、IngestRetention、JudgmentRetention 是本人没在设置里改过时的保留期（配置或默认）；
	// 本人在设置里改过的存在账本里，每次清理时读取，优先于它们。
	// RunRetention 是运行目录保留期，默认 30 天（config.Default）。
	RunRetention    time.Duration
	IngestRetention time.Duration
	// JudgmentRetention 是判断记录保留期，默认 180 天（config.Default）。
	JudgmentRetention time.Duration
	// KeepJudgments 为真时不清理判断记录（配置 judgment_retention_days: 0）。
	KeepJudgments bool
	Now           func() time.Time
}

// Service 执行清理；Sweep 幂等，可反复调用。
type Service struct{ o Options }

func New(o Options) (*Service, error) {
	if o.Store == nil || o.Ingest == nil || o.Exec == nil || o.Layout.Runs == "" {
		return nil, errors.New("retention: Store, Ingest, Exec and Layout are required")
	}
	if o.RunRetention <= 0 {
		o.RunRetention = config.Default().RunRetention
	}
	if o.IngestRetention <= 0 {
		o.IngestRetention = config.Default().IngestRetention
	}
	if o.JudgmentRetention <= 0 {
		o.JudgmentRetention = config.Default().JudgmentRetention
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	return &Service{o: o}, nil
}

// Report 是一次清理的报数。
type Report struct {
	At              time.Time `json:"at"`
	RunDirs         int       `json:"run_dirs"`          // 删除的运行目录数
	RunDirBytes     int64     `json:"run_dir_bytes"`     // 这些目录共多少字节
	IngestBodies    int64     `json:"ingest_bodies"`     // 清掉正文的请求体条数
	Judgments       int64     `json:"judgments"`         // 删除的判断记录条数
	SkippedInFlight []string  `json:"skipped_in_flight"` // 目录还在、因在途而没动的运行 id
	Error           string    `json:"error,omitempty"`
}

// Sweep 先清 Ingest 请求体，再清运行目录、判断记录和来源原文；互不影响，错误合并返回。
// 保留天数每次从账本读，设置里改了下一次就按新的算；0 不清这一项。清完把报数记入账本。
func (s *Service) Sweep(ctx context.Context) (Report, error) {
	rep := Report{At: s.o.Now(), SkippedInFlight: []string{}}
	var ov map[string]int
	err := s.o.Store.View(ctx, func(tx *store.Tx) (e error) { ov, e = overrides(tx); return })
	if err != nil {
		return rep, fmt.Errorf("retention: read settings: %w", err)
	}
	keep := func(key string, def time.Duration) time.Duration {
		if d, ok := ov[key]; ok {
			return days(d)
		}
		return def
	}
	judgment := s.o.JudgmentRetention
	if s.o.KeepJudgments {
		judgment = 0
	}
	var errs []error
	if d := keep(keyIngest, s.o.IngestRetention); d > 0 {
		n, err := s.o.Ingest.ExpireOlderThan(ctx, d)
		rep.IngestBodies = n
		errs = append(errs, err)
	}
	if d := keep(keyRun, s.o.RunRetention); d > 0 {
		errs = append(errs, s.expireRunDirs(ctx, d, &rep))
	}
	if d := keep(keyJudgment, judgment); d > 0 {
		errs = append(errs, s.expireJudgments(ctx, d, &rep))
	}
	errs = append(errs, s.ExpireContent(ctx))
	err = errors.Join(errs...)
	if err != nil {
		rep.Error = err.Error()
	}
	body, _ := json.Marshal(rep)
	if _, e := s.o.Exec.Do(ctx, command.Request{Identity: system, Command: CmdReport, Input: body}); e != nil {
		err = errors.Join(err, fmt.Errorf("retention: record report: %w", e))
	}
	return rep, err
}

func (s *Service) expireJudgments(ctx context.Context, keep time.Duration, rep *Report) error {
	cut := s.o.Now().Add(-keep)
	err := s.o.Store.Update(ctx, func(tx *store.Tx) (err error) { rep.Judgments, err = record.DeleteBefore(tx, cut); return })
	if err != nil {
		return fmt.Errorf("retention: expire judgments: %w", err)
	}
	return nil
}

// expireRunDirs 删除已结束超过保留期的运行目录。只看终态 Run 的结束时间：
// 排队或运行中的 Run 无论多久都不动（目录还在的记入 SkippedInFlight）；Run 记录本身留在账本。
// 只统计这一次真正删掉的目录，所以连跑第二次是 0。
func (s *Service) expireRunDirs(ctx context.Context, keep time.Duration, rep *Report) error {
	cut := s.o.Now().Add(-keep)
	var recs []proc.Record
	if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { recs, err = proc.List(tx); return }); err != nil {
		return fmt.Errorf("retention: list runs: %w", err)
	}
	var errs []error
	for _, r := range recs {
		// 运行 id 来自账本，仍要确认它只是 runs/ 下的一级目录名，防止 id 带路径逃出数据目录。
		if r.Run.ID == "" || r.Run.ID != filepath.Base(r.Run.ID) || r.Run.ID == "." || r.Run.ID == ".." {
			errs = append(errs, fmt.Errorf("retention: run id %q is not a plain directory name", r.Run.ID))
			continue
		}
		dir := s.o.Layout.RunDir(r.Run.ID)
		if !r.Run.State.Terminal() {
			if _, err := os.Stat(dir); err == nil {
				rep.SkippedInFlight = append(rep.SkippedInFlight, r.Run.ID)
			}
			continue
		}
		if r.Run.FinishedAt == nil || !r.Run.FinishedAt.Before(cut) {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue // 已经删过
		}
		size := dirBytes(dir)
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("retention: remove run dir %s: %w", r.Run.ID, err))
			continue
		}
		rep.RunDirs++
		rep.RunDirBytes += size
	}
	return errors.Join(errs...)
}

func dirBytes(dir string) (n int64) {
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// Next 返回 now 之后的下一个 at（一天里的时分）；now 恰在 at 时取明天，所以同一时刻不会触发两次。
func Next(now time.Time, hour, min int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Run 启动时清一次，之后每天 hour:minute 清一次，直到 ctx 结束。清理出错只交给 report，不退出。
// changed 是时钟被拨动的通知（测试构建）；nil 时只靠计时器。
func (s *Service) Run(ctx context.Context, hour, minute int, changed func() <-chan struct{}, report func(Report, error)) {
	sweep := func() { r, err := s.Sweep(ctx); report(r, err) }
	sweep()
	next := Next(s.o.Now(), hour, minute)
	for {
		// 最久睡一分钟再看时钟：休眠或系统时间被拨动后，不会错过 04:10 太久。
		wait := time.NewTimer(min(max(next.Sub(s.o.Now()), 0), time.Minute))
		var ch <-chan struct{}
		if changed != nil {
			ch = changed()
		}
		select {
		case <-ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
		case <-ch:
			wait.Stop()
		}
		if now := s.o.Now(); !now.Before(next) {
			sweep()
			next = Next(now, hour, minute)
		}
	}
}
