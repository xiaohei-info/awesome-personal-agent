// Package runidentity 是运行凭据（设计 §14.2、§14.3、§7.7）：内核为每次 agent 运行（任务型与后台运行）
// 生成的 worker 级短期凭据。它绑定一次运行及其目标事项，范围是目标事项与其子事项（含更深的后代），
// 没有代答能力（决策、建议、声明命令一律退出码 4，由授权判定的 LevelRun 行保证），不是新的信任级。
//
// 凭据明文只在签发时返回一次，账本只存 SHA-256 哈希。凭据随运行结束失效：每次认证都读运行状态，
// 终态（成功、失败、超时）即拒绝，所以失效与 proc.Settle 在同一事务生效，不依赖调用方记得吊销；
// Revoke 提供显式吊销。首版经环境变量（Env）和运行上下文文件（kernel/outcall/proc）传递；只传凭据的文件方式不在本包（§7.7，接入 dsh 时再做）。
// 唤醒回合的投递不带明文，由网关插件用自己的 token 经 Exchange 换取，这样换出的凭据另有过期时间（§13.6）。
package runidentity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const prefix = "apa_run_"

// LaunchWake 是 domain.Run.Launch 的唤醒取值（§13.6）。
const LaunchWake = "wake"

// IsSecret 报告 Bearer 明文是否是运行凭据的形态（入口据此选择认证方式）。
func IsSecret(s string) bool { return strings.HasPrefix(s, prefix) }

// Module 是本模块在存储层登记的迁移。一次运行一行：重新签发（续接）覆盖旧哈希，旧明文随即失效。
var Module = store.Module{Name: "runidentity", Migrations: []store.Migration{{
	Version: 1, Name: "create-run-credential",
	SQL: `CREATE TABLE runidentity_credential (
	run_id TEXT PRIMARY KEY,
	hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER NOT NULL DEFAULT 0
);`,
}, {
	Version: 2, Name: "add-credential-expiry",
	SQL: `ALTER TABLE runidentity_credential ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0;`,
}}}

// EvExchanged 记一次唤醒运行的凭据换取；不含明文，不在对外稳定的事件类型表里。
const EvExchanged = "run.credential_exchanged"

// DefaultExchangeTTL 是运行未设超时时，换取凭据的有效期。
const DefaultExchangeTTL = time.Hour

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the run request")
}

func unauthenticated() error {
	return domain.NewError(domain.CodeUnauthenticated, "credential is missing, unknown, revoked or expired", "the run has ended or the credential is invalid; do not retry")
}

func hashOf(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// runItem 取 agent 运行及其目标事项。来源拉取、转换、读取等不是 agent 运行，目标也不是事项，不签发。
func runItem(tx *store.Tx, runID string) (proc.Record, domain.Item, error) {
	rec, err := proc.Get(tx, runID)
	if err != nil {
		return rec, domain.Item{}, err
	}
	if c := rec.Run.Category; c != domain.RunTask && c != domain.RunBackground {
		return rec, domain.Item{}, invalid("only task and background runs get a run credential")
	}
	it, err := items.Get(tx, rec.Run.Target)
	if err != nil {
		if e, ok := err.(*domain.Error); ok && e.Code == domain.CodeNotFound {
			return rec, it, invalid("run target is not an existing item")
		}
		return rec, it, err
	}
	return rec, it, nil
}

// Issue 在 tx 内为运行签发凭据并返回明文（唯一一次可见）。运行必须是未结束的 task 或 background 运行，
// 目标事项必须存在。重复签发替换旧凭据。调用方应与 Run 入队放在同一事务（§13.6 拉起流程第 3 步）。
func Issue(tx *store.Tx, runID string, now time.Time) (string, error) {
	return issue(tx, runID, now, time.Time{})
}

// issue 的 expires 为零值表示随运行结束失效，不另设过期。
func issue(tx *store.Tx, runID string, now, expires time.Time) (string, error) {
	rec, _, err := runItem(tx, runID)
	if err != nil {
		return "", err
	}
	if rec.Run.State.Terminal() {
		return "", domain.NewError(domain.CodeConflict, "run has already finished", "start a new run to get a new credential")
	}
	b := make([]byte, 32) // 256 位熵，快哈希足够
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := prefix + base64.RawURLEncoding.EncodeToString(b)
	var exp int64
	if !expires.IsZero() {
		exp = expires.UnixNano()
	}
	if _, err := tx.Exec(`INSERT INTO runidentity_credential (run_id, hash, created_at, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET hash = excluded.hash, created_at = excluded.created_at, revoked_at = 0, expires_at = excluded.expires_at`,
		runID, hashOf(secret), now.UnixNano(), exp); err != nil {
		return "", err
	}
	return secret, nil
}

// Exchange 把唤醒运行的 id 换成运行凭据（§13.6），在 tx 内记 Event。只接受 worker、delegate token（网关插件），
// 只对未结束的 launch=wake 后台运行有效。再次换取时旧凭据立即作废。
func Exchange(tx *store.Tx, c command.Caller, runID string, now time.Time) (string, time.Time, error) {
	if c.Kind != auth.Worker && c.Kind != auth.Delegate {
		return "", time.Time{}, domain.NewError(domain.CodePermissionDenied, "only a gateway plugin token can exchange a run id for a run credential", "use the plugin's worker or delegate token")
	}
	rec, err := proc.Get(tx, runID)
	if err != nil {
		return "", time.Time{}, err
	}
	if rec.Run.Category != domain.RunBackground || rec.Run.Launch != LaunchWake {
		return "", time.Time{}, domain.NewError(domain.CodeConflict, "run "+runID+" is not a wake run", "only wake runs get their credential by exchange")
	}
	ttl := DefaultExchangeTTL
	if rec.Run.TimeoutSec > 0 {
		ttl = time.Duration(rec.Run.TimeoutSec) * time.Second
	}
	expires := now.Add(ttl)
	secret, err := issue(tx, runID, now, expires)
	if err != nil {
		return "", time.Time{}, err
	}
	payload, _ := json.Marshal(map[string]string{"run_id": runID, "expires_at": expires.UTC().Format(time.RFC3339)})
	if _, err := eventlog.Append(tx, domain.Event{Type: EvExchanged, Object: runID, Actor: c.Actor, Payload: payload, ChainDepth: rec.Run.ChainDepth, At: now}); err != nil {
		return "", time.Time{}, err
	}
	return secret, expires, nil
}

// Revoke 显式吊销运行的凭据，立即生效；重复吊销不报错。运行不存在返回 not_found。
func Revoke(tx *store.Tx, runID string) error {
	if _, err := proc.Get(tx, runID); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE runidentity_credential SET revoked_at = 1 WHERE run_id = ? AND revoked_at = 0", runID)
	return err
}

// Identity 是运行凭据入口，实现 command.Identity：把请求携带的明文认证成 worker 级的运行身份。
type Identity struct {
	Secret string
	Origin auth.Origin
	Now    func() time.Time // 判断过期用；空取系统时钟
}

// Authenticate 校验明文：未知、已吊销、已过期、运行已结束统一返回 unauthenticated（退出码 3），不泄露区别。
func (i Identity) Authenticate(tx *store.Tx) (command.Caller, error) {
	if len(i.Secret) <= len(prefix) || i.Secret[:len(prefix)] != prefix {
		return command.Caller{}, unauthenticated()
	}
	runID, revoked, expires, found, err := lookup(tx, hashOf(i.Secret))
	if err != nil {
		return command.Caller{}, err
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	if !found || revoked != 0 || (expires != 0 && now().UnixNano() >= expires) {
		return command.Caller{}, unauthenticated()
	}
	rec, target, err := runItem(tx, runID)
	if err != nil {
		return command.Caller{}, err
	}
	if rec.Run.State.Terminal() {
		return command.Caller{}, unauthenticated()
	}
	harness := i.Origin.Harness
	if harness == "" {
		harness = rec.Run.Harness
	}
	return command.Caller{
		Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run:" + runID, Via: domain.ViaCLI,
			Harness: harness, Session: i.Origin.Session, Run: runID},
		Level:    authz.LevelRun,
		RunScope: scope(target),
		RunDepth: rec.Run.ChainDepth,
	}, nil
}

func lookup(tx *store.Tx, hash string) (runID string, revoked, expires int64, found bool, err error) {
	rows, err := tx.Query("SELECT run_id, revoked_at, expires_at FROM runidentity_credential WHERE hash = ?", hash)
	if err != nil {
		return "", 0, 0, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", 0, 0, false, rows.Err()
	}
	err = rows.Scan(&runID, &revoked, &expires)
	return runID, revoked, expires, err == nil, err
}

// scope 解析对象是否在运行范围内：对象是目标事项或其后代；项目是否为目标事项所属项目。
// 范围全部来自账本（parent 关系与事项的项目），不来自请求。
func scope(target domain.Item) func(*store.Tx, authz.Object) (bool, bool, error) {
	return func(tx *store.Tx, o authz.Object) (bool, bool, error) {
		inProject := target.Project != "" && o.Project == target.Project
		seen := map[string]bool{}
		for id := o.ID; id != "" && !seen[id]; {
			if id == target.ID {
				return true, inProject, nil
			}
			seen[id] = true
			parent, ok, err := items.ParentOf(tx, id)
			if err != nil {
				return false, false, err
			}
			if !ok {
				break
			}
			id = parent
		}
		return false, inProject, nil
	}
}

// Env 是运行凭据的环境变量传递（§7.7）：供 proc.Options.Env 在启动时注入，不入账本。
// project 是目标事项所属项目，为空则不注入 APA_PROJECT。
func Env(apiURL, secret string, run domain.Run, project string) map[string]string {
	env := map[string]string{"APA_URL": apiURL, "APA_TOKEN": secret, "APA_RUN": run.ID}
	if project != "" {
		env["APA_PROJECT"] = project
	}
	return env
}
