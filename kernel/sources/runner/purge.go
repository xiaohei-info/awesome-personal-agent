package runner

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 来源原文的保留与物理删除（§6.15）。来源事件与原文表只由本包写：保留期清除（kernel/retention）与物理删除（kernel/purge）
// 经下面的函数改，不直写这两张表。授权、审计事件与 blob 文件的删除在调用方。

// tombstoned 报告去重键是否是物理删除留下的墓碑。
func tombstoned(tx *store.Tx, k domain.DedupeKey) (bool, error) {
	return exists(tx, `SELECT 1 FROM sources_runner_tombstone WHERE dedupe_domain = ? AND event_id = ?`, k.Domain, k.EventID)
}

func exists(tx *store.Tx, q string, args ...any) (bool, error) {
	r, err := tx.Query(q, args...)
	if err != nil {
		return false, err
	}
	defer r.Close()
	found := r.Next()
	return found, r.Err()
}

// attachmentBlobs 读事件附件清单里已取回的附件的 blob 哈希（清单项的 blob；带 apa://blob/ 前缀的也认）。
func attachmentBlobs(tx *store.Tx, id string) ([]string, error) {
	r, err := tx.Query(`SELECT CAST(content AS TEXT) FROM sources_runner_event_part WHERE event_id = ? AND part = 'attachments'`, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []string
	for r.Next() {
		var raw string
		if err := r.Scan(&raw); err != nil {
			return nil, err
		}
		var list []struct {
			Blob string `json:"blob"`
		}
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, err
		}
		for _, a := range list {
			if h, err := blobs.ParseRef(blobs.Ref(strings.TrimPrefix(a.Blob, blobs.RefPrefix))); err == nil { // 不是哈希的值不当 blob 处理
				out = append(out, h)
			}
		}
	}
	return out, r.Err()
}

// BlobReferenced 报告是否还有来源事件的附件清单引用这个 blob 哈希。
func BlobReferenced(tx *store.Tx, hash string) (bool, error) {
	return exists(tx, `SELECT 1 FROM sources_runner_event_part WHERE part = 'attachments' AND instr(CAST(content AS TEXT), ?) > 0 LIMIT 1`, hash)
}

// EventsOfItem 返回挂在事项上（item 或 items[] 含它）的来源事件 id。
func EventsOfItem(tx *store.Tx, item string) ([]string, error) {
	return ids(tx, `WHERE item = ? OR EXISTS (SELECT 1 FROM json_each(CASE WHEN items = '' THEN '[]' ELSE items END) WHERE value = ?)`, item, item)
}

// PurgeEvent 物理删除一条来源事件：事件行与原文各部分都删除，只留去重键作墓碑。
// 返回它的附件清单里已取回的 blob 哈希，由调用方在确认没有其他引用后删除文件；判断记录由调用方按事件 id 删除。
func PurgeEvent(tx *store.Tx, id string) (blobs []string, err error) {
	ev, err := Event(tx, id)
	if err != nil {
		return nil, err
	}
	if blobs, err = attachmentBlobs(tx, id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO sources_runner_tombstone (dedupe_domain, event_id) VALUES (?, ?)`, ev.DedupeKey.Domain, ev.DedupeKey.EventID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`DELETE FROM sources_runner_event_part WHERE event_id = ?`, id); err != nil {
		return nil, err
	}
	_, err = tx.Exec(`DELETE FROM sources_runner_event WHERE id = ?`, id)
	return blobs, err
}

// Expired 是保留期已到的来源事件和它挂靠的事项（事项 id 不含空、不重复）。
type Expired struct {
	ID    string
	Items []string
}

// ExpiredContent 返回某来源落盘早于 before、原文还在的事件。还没处置（pending）与未判定（unjudged）的事件不在其中：
// 入站流水线还要读它们的原文。是否挂着未关闭事项由调用方判定（事项属于别的模块）。
func ExpiredContent(tx *store.Tx, sourceID string, before time.Time) ([]Expired, error) {
	r, err := tx.Query(`SELECT id, item, items FROM sources_runner_event
		WHERE source_id = ? AND content_state <> 'purged' AND disposition NOT IN ('pending', 'unjudged')
		AND COALESCE(ingested_at, occurred_at) < ? ORDER BY seq`, sourceID, before.UnixNano())
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Expired
	for r.Next() {
		var e Expired
		var item, items string
		if err := r.Scan(&e.ID, &item, &items); err != nil {
			return nil, err
		}
		if item != "" {
			e.Items = append(e.Items, item)
		}
		var more []string
		if items != "" {
			if err := json.Unmarshal([]byte(items), &more); err != nil {
				return nil, err
			}
		}
		for _, m := range more {
			if m != "" && m != item {
				e.Items = append(e.Items, m)
			}
		}
		out = append(out, e)
	}
	return out, r.Err()
}

// PurgeContent 清掉一条事件的原文各部分与预览，事件行留下类型、标题、发送者、时间、链接与处置，完整程度标为 purged。
// 返回清掉的字节数与附件清单里已取回的 blob 哈希（调用方在确认没有其他引用后删除文件）。
func PurgeContent(tx *store.Tx, id string) (n int64, blobs []string, err error) {
	if blobs, err = attachmentBlobs(tx, id); err != nil {
		return 0, nil, err
	}
	// 新落盘的事件字节数在原文表里；升级前落盘的（legacy）没有原文表的行，字节数按旧列的正文与聚合消息现算。
	r, err := tx.Query(`SELECT COALESCE(NULLIF((SELECT SUM(bytes) FROM sources_runner_event_part WHERE event_id = e.id), 0),
			length(CAST(detail AS BLOB)) + length(CAST(messages AS BLOB))) FROM sources_runner_event e WHERE id = ?`, id)
	if err != nil {
		return 0, nil, err
	}
	if r.Next() {
		err = r.Scan(&n)
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, nil, err
	}
	if _, err = tx.Exec(`DELETE FROM sources_runner_event_part WHERE event_id = ?`, id); err != nil {
		return 0, nil, err
	}
	_, err = tx.Exec(`UPDATE sources_runner_event SET detail = '', messages = '', content_state = 'purged' WHERE id = ?`, id)
	return n, blobs, err
}
