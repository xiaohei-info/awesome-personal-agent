package doctor

import (
	"context"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ExtKey 是一把未吊销的 ext 密钥（不含密钥本身和哈希）。
type ExtKey struct {
	ID    string
	Scope string
	Label string
}

// Facts 是自检在一个只读事务里读到的账本事实。检查本身不再碰账本，探测（外呼）因此不占着账本事务。
type Facts struct {
	Policy       policy.Policy
	Sources      []domain.Source
	Declarations []executors.Record // 生效的声明
	ExtKeys      []ExtKey
	Seen         map[string]time.Time // 见 Seen：subject → 最近一次见到
	Backlog      map[string]int       // 投递路由 → 还没送出的投递数
}

// ReadFacts 读取自检需要的账本事实；账本结构必须是当前二进制的结构（旧结构用 LegacySources）。
func ReadFacts(ctx context.Context, st *store.Store) (f Facts, err error) {
	err = st.View(ctx, func(tx *store.Tx) error {
		if _, f.Policy, err = policy.Current(tx); err != nil {
			return err
		}
		if f.Sources, err = registry.List(tx); err != nil {
			return err
		}
		if f.Declarations, err = executors.Records(tx, executors.StateActive); err != nil {
			return err
		}
		creds, err := auth.New(nil).List(tx)
		if err != nil {
			return err
		}
		for _, c := range creds {
			if c.Kind == auth.Ext && c.RevokedAt.IsZero() {
				f.ExtKeys = append(f.ExtKeys, ExtKey{ID: c.ID, Scope: c.Scope, Label: c.Label})
			}
		}
		if f.Seen, err = readSeen(tx); err != nil {
			return err
		}
		backlog, err := delivery.New(delivery.Options{}).Backlog(tx)
		if err != nil {
			return err
		}
		f.Backlog = map[string]int{}
		for _, d := range backlog {
			f.Backlog[d.Route]++
		}
		return nil
	})
	return f, err
}

// LegacySources 读来源登记，兼容还没迁到账本结构 3 的账本（没有外发范围列）：这时按迁移规则，
// 本机范围为 full、外部范围未选择（设计 §16.5 "已有来源"）。升级预检用它在不迁移的情况下报告将来会是什么样。
func LegacySources(ctx context.Context, st *store.Store) (out []domain.Source, err error) {
	err = st.View(ctx, func(tx *store.Tx) error {
		cols, err := columns(tx, "sources_registry_source")
		if err != nil {
			return err
		}
		if cols["outbound_external"] {
			out, err = registry.List(tx)
			return err
		}
		r, err := tx.Query(`SELECT id, type, source_type, command, enabled, stats FROM sources_registry_source ORDER BY id`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var s domain.Source
			var typ, stats string
			var enabled int64
			if err := r.Scan(&s.ID, &typ, &s.SourceKind, &s.Command, &enabled, &stats); err != nil {
				return err
			}
			s.Type, s.Enabled, s.Outbound = domain.SourceType(typ), enabled != 0, domain.Outbound{Local: "full"}
			if stats != "" {
				s.Stats = []byte(stats)
			}
			out = append(out, s)
		}
		return r.Err()
	})
	return out, err
}

func columns(tx *store.Tx, table string) (map[string]bool, error) {
	r, err := tx.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]bool{}
	for r.Next() {
		var n string
		if err := r.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, r.Err()
}

// ExternalInUse 报告是否配置了位置为 external（含未声明）的模型端点：只有这时外部范围才起作用（设计 §14.14）。
func ExternalInUse(p policy.Policy) bool {
	if je := p.JudgeEndpoint; je != nil && je.URL != "" && policy.EffectiveLocality(je.Locality) == policy.LocalityExternal {
		return true
	}
	g := p.GenerateEndpoint
	return g.URL != "" && policy.EffectiveLocality(g.Locality) == policy.LocalityExternal
}

// ChannelName 取 ext 密钥 scope 里的渠道名；不是渠道密钥返回空。
func ChannelName(scope string) string {
	n, _ := strings.CutPrefix(scope, "channel.")
	if n == scope {
		return ""
	}
	return n
}
