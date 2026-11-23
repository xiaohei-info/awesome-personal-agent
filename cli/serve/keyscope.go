package serve

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	sregistry "github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// EvScopesMigrated 记钥匙逐来源档位的一次性迁移（§16.5）：载荷逐把列出换算结果。
const EvScopesMigrated = "credential.scopes_migrated"

var keyScopeActor = domain.Actor{Type: domain.ActorSystem, Principal: "key-scope-migration", Via: domain.ViaWeb}

// legacyTokenScope 是升级前 token 读该来源原文实际拿到的档位（旧 api/sources.readScope）：
// 外发范围 full、excerpt 照给，meta、none、未选择都只回元信息。
func legacyTokenScope(s domain.Source) string {
	if e := s.Outbound.External; e != nil && (*e == sregistry.ScopeFull || *e == sregistry.ScopeExcerpt) {
		return *e
	}
	return sregistry.ScopeMeta
}

// migrateKeyScopes 把升级前签发、source_scopes 还是空串的钥匙换算成逐来源档位，启动时在迁移之后运行，幂等、一个事务：
// observer、worker、delegate token 按它们今天经 token 读原文实际拿到的档位逐来源写入（不放宽）；ext、webhook 写空表
// （它们原本读不了来源）。audience 一律保持缺省 external，内部只由本人在 Web 标（§14.2）。
func migrateKeyScopes(ctx context.Context, st *store.Store, now func() time.Time) error {
	return st.Update(ctx, func(tx *store.Tx) error {
		a := auth.New(now)
		todo, err := a.Unmigrated(tx)
		if err != nil || len(todo) == 0 {
			return err
		}
		srcs, err := sregistry.List(tx)
		if err != nil {
			return err
		}
		token := map[string]string{}
		for _, s := range srcs {
			token[s.ID] = legacyTokenScope(s)
		}
		done := map[string]map[string]string{}
		for _, c := range todo {
			scopes := map[string]string{}
			if c.Kind == auth.Observer || c.Kind == auth.Worker || c.Kind == auth.Delegate {
				scopes = token
			}
			if err := a.MigrateScopes(tx, c.ID, scopes); err != nil {
				return err
			}
			done[c.ID] = scopes
		}
		payload, err := json.Marshal(map[string]any{"credentials": done})
		if err != nil {
			return err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: EvScopesMigrated, Object: "settings:credentials", Actor: keyScopeActor,
			Reason: "key source scopes migrated from the source outbound scope", Payload: payload, At: now()}); err != nil {
			return err
		}
		slog.Info("key source scopes migrated", "credentials", len(done))
		return nil
	})
}
