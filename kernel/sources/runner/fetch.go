package runner

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 附件取回（§7.6、§9.2 的 fetch 模式）：本人按需取回清单里的一个附件。运行本身不落事件、不动游标；
// 内核只在取回成功后把 blob 记进清单。

const (
	// FetchPrefix 是 fetch 模式那次运行的 Target 前缀：结果由发起方同步取走，不是拉取结果（Handle 忽略它）。
	FetchPrefix = "fetch:"
	// FetchTimeout 是 fetch 模式一次运行的超时（秒）。
	FetchTimeout = 60
	// MaxFetchBytes 是取回的单个附件的上限；超过即失败，清单不变。
	MaxFetchBytes = 32 << 20
)

type fetchAttachment struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

// fetchDoc 是 fetch 模式的 stdin：EventID 是来源程序自己给这条事件的 id（去重键里的那个），ref 是它自己认得的取回句柄。
type fetchDoc struct {
	SourceID        string          `json:"source_id"`
	Config          json.RawMessage `json:"config"`
	CredentialsPath string          `json:"credentials_path,omitempty"`
	EventID         string          `json:"event_id"`
	Attachment      fetchAttachment `json:"attachment"`
}

// FetchPlan 是来源 s 的 fetch 模式：命令是程序名加 fetch，工作目录同拉取；走短任务队列，本人发起所以排在前面。
// ev 是附件所在的来源事件，att 是清单里的那一项（须有 Ref）。stdout 是附件的字节，按 MaxFetchBytes 限长。
func (r *Runner) FetchPlan(s domain.Source, ev domain.SourceEvent, n int, att AttachmentRef, now time.Time) (proc.Request, error) {
	if att.Ref == "" {
		return proc.Request{}, domain.NewError(domain.CodeConflict, "attachment has no fetch handle", "the source program gave no ref for it")
	}
	run, spec, err := r.plan(s, Stats{}, now)
	if err != nil {
		return proc.Request{}, err
	}
	doc := fetchDoc{SourceID: s.ID, Config: s.Config, CredentialsPath: s.CredentialsPath, EventID: ev.DedupeKey.EventID,
		Attachment: fetchAttachment{Ref: att.Ref, Name: att.Name}}
	if spec.Stdin, err = json.Marshal(doc); err != nil {
		return proc.Request{}, err
	}
	spec.Command = []string{spec.Command[0], "fetch"}
	spec.StdoutLimit = MaxFetchBytes
	run.Target, run.TimeoutSec = fmt.Sprintf("%s%s:%s:%d", FetchPrefix, s.ID, ev.ID, n), FetchTimeout
	return proc.Request{Run: run, Spec: spec, Explicit: true}, nil
}

// EventAttachments 读事件的附件清单（第 n 项从 0 数起）。没有这个部分（来源没带、升级前没存、已按保留期清除）是 not_found。
func EventAttachments(tx *store.Tx, eventID string) ([]AttachmentRef, error) {
	pd, err := ReadPart(tx, eventID, PartAttachments)
	if err != nil {
		return nil, err
	}
	var list []AttachmentRef
	if err := json.Unmarshal(pd.Content, &list); err != nil {
		return nil, fmt.Errorf("runner: event %s attachments: %w", eventID, err)
	}
	return list, nil
}

// SetAttachmentBlob 在调用方事务里把第 n 个附件记上 blob（内容的 sha256），返回更新后的清单项（含来源程序给的全部字段）。
// 清单是原文的一部分：部分的字节数、sha256 与事件行的原文字节数随之改变；清单里其他字段原样保留。
// 部分不存在或 n 越界是 not_found；这一项没有取回句柄是 conflict。
func SetAttachmentBlob(tx *store.Tx, eventID string, n int, hash string) (json.RawMessage, error) {
	pd, err := ReadPart(tx, eventID, PartAttachments)
	if err != nil {
		return nil, err
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(pd.Content, &raw); err != nil {
		return nil, fmt.Errorf("runner: event %s attachments: %w", eventID, err)
	}
	if n < 0 || n >= len(raw) {
		return nil, domain.NewError(domain.CodeNotFound, fmt.Sprintf("source event %s has no attachment %d", eventID, n), "read the content manifest for the attachments it has")
	}
	raw[n]["blob"], _ = json.Marshal(blobs.Ref(hash))
	updated, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE sources_runner_event_part SET content = ?, bytes = ?, sha256 = ? WHERE event_id = ? AND part = ?`,
		updated, int64(len(updated)), shaHex(updated), eventID, PartAttachments); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE sources_runner_event SET content_bytes = COALESCE(content_bytes, 0) + ? WHERE id = ?`,
		int64(len(updated))-int64(len(pd.Content)), eventID); err != nil {
		return nil, err
	}
	return json.Marshal(raw[n])
}
