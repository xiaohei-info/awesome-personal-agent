// Package runidentity 是运行凭据（设计 §14.2、§14.3、§7.7）：内核为每次 agent 运行（任务型与后台运行）
// 生成的短期凭据。它只带身份：哪次运行、属于哪个 Agent、链路深度。它不带写范围或读取限制，
// 判定结果与所属 Agent 的钥匙发出的相同；运行的目标不是事项时照常签发。
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
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

// agentRun 取 agent 运行。来源拉取、转换、读取等不是 agent 运行，不签发；目标是不是事项不影响签发。
func agentRun(tx *store.Tx, runID string) (proc.Record, error) {
	rec, err := proc.Get(tx, runID)
	if err != nil {
		return rec, err
	}
	// 来源批处理（source_batch）的目标不是事项，凭据同样只带运行与 Agent 身份（共享契约的接缝）。
	if c := rec.Run.Category; c != domain.RunTask && c != domain.RunBackground && c != domain.RunSourceBatch {
		return rec, invalid("only task, background and source batch runs get a run credential")
	}
	return rec, nil
}

// Issue 在 tx 内为运行签发凭据并返回明文（唯一一次可见）。运行必须是未结束的 task、background 或 source_batch 运行；目标不必是事项。
// 重复签发替换旧凭据。调用方应与 Run 入队放在同一事务（§13.6 拉起流程第 3 步）。
func Issue(tx *store.Tx, runID string, now time.Time) (string, error) {
	return issue(tx, runID, now, time.Time{})
}

// issue 的 expires 为零值表示随运行结束失效，不另设过期。
func issue(tx *store.Tx, runID string, now, expires time.Time) (string, error) {
	rec, err := agentRun(tx, runID)
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
	if rec.Run.InstanceID() != c.CredentialAgentID {
		return "", time.Time{}, domain.NewError(domain.CodePermissionDenied, "gateway is not bound to this Agent", "reload Agent settings")
	}
	ttl := DefaultExchangeTTL
	if rec.Run.TimeoutSec > 0 {
		ttl = time.Duration(rec.Run.TimeoutSec) * time.Second
	}
	// 截止时刻是运行开始时间加超时（§13.6），不是换取时刻；尚未开始则以换取时刻为开始。
	start := now
	if rec.Run.StartedAt != nil {
		start = *rec.Run.StartedAt
	}
	expires := start.Add(ttl)
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
	now := clock.Now
	if i.Now != nil {
		now = i.Now
	}
	if !found || revoked != 0 || (expires != 0 && now().UnixNano() >= expires) {
		return command.Caller{}, unauthenticated()
	}
	rec, err := agentRun(tx, runID)
	if err != nil {
		return command.Caller{}, err
	}
	if rec.Run.State.Terminal() {
		return command.Caller{}, unauthenticated()
	}
	agentID := rec.Run.InstanceID()
	harness := i.Origin.Harness
	if harness == "" {
		harness = rec.Run.Harness
	}
	return command.Caller{
		Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run:" + runID, Via: domain.ViaCLI,
			Harness: harness, Session: i.Origin.Session, Run: runID},
		Level:    authz.LevelRun,
		RunDepth: rec.Run.ChainDepth,
		// 权限随所属 Agent（运行记录上的快照）；Actor.Harness 可以是调用方自报的，只作留痕（§14.3）。
		RunAgentID: agentID,
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

// Env 是运行凭据的环境变量传递（§7.7）：供 proc.Options.Env 在启动时注入，不入账本。
// project 是目标事项所属项目，为空则不注入 APA_PROJECT。
func Env(apiURL, secret string, run domain.Run, project string) map[string]string {
	env := map[string]string{"APA_URL": apiURL, "APA_TOKEN": secret, "APA_RUN": run.ID}
	if project != "" {
		env["APA_PROJECT"] = project
	}
	return env
}
