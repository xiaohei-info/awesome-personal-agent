package items

import (
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 导入（§7.1）：事项中心拥有导入批次与导入对照两张表和导入写入的投影；
// 记录的解析、校验与批次编排在 kernel/imports，它只经本文件的入口写事项。

// 导入历史的三种步骤。
const (
	StepCreated    = "created"
	StepTransition = "transition"
	StepProgress   = "progress"
)

// ImportStep 是导入历史的一步；At 是原始时刻。
type ImportStep struct {
	At        time.Time
	Event     string       // StepCreated、StepTransition、StepProgress
	Phase     domain.Phase // created：初始阶段；transition：目标阶段
	Reason    string
	Text, On  string // progress
	Precision string // 空或 day
}

// ImportedItem 是一条已校验的导入事项：Item 是最终取值（Phase 是历史走完后的阶段，Origin 已定），
// Steps 按 At 排好序且第一步是 created。
type ImportedItem struct {
	Item  domain.Item
	Steps []ImportStep
}

// CheckImported 用普通写入同一套校验（字段、项目存在、waiting_on 形状、阶段不变量、kind 的 spec schema）检查导入事项的一个阶段快照。
func (k *Kinds) CheckImported(tx *store.Tx, it domain.Item) error {
	return (&svc{kinds: k}).check(tx, it)
}

// snapshotAt 是历史走到某一阶段时的事项快照：等待对象只在 waiting 阶段存在（同 transit）。
func snapshotAt(final domain.Item, phase domain.Phase) domain.Item {
	it := final
	it.Phase = phase
	if phase != domain.PhaseWaiting {
		it.WaitingOn = nil
	}
	return it
}

// WriteImported 在命令的事务里写一条导入事项：按历史逐步追加事件（时刻取原始时刻，带导入标记与实际写入时刻），
// 每条事件对应一次版本加一，投影、进展与截止 Timer 在同一事务内更新。返回新事项的 id。
func WriteImported(c *command.Ctx, im ImportedItem, info domain.ImportInfo) (string, error) {
	if len(im.Steps) == 0 || im.Steps[0].Event != StepCreated {
		return "", invalid("import history must start with created")
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	final := im.Item
	final.ID, final.ChainDepth, final.Imported = id, c.ChainDepth, &info
	var cur domain.Item
	nprog := 0
	for _, st := range im.Steps {
		var (
			typ, reason string
			pl          payload
		)
		switch st.Event {
		case StepCreated:
			cur = stamped(snapshotAt(final, st.Phase), st.At)
			typ, reason = domain.EvItemCreated, st.Reason
			pl = payload{Op: "create", Item: cur}
		case StepTransition:
			before := cur
			cur = stamped(snapshotAt(final, st.Phase), st.At)
			cur.CreatedAt = before.CreatedAt
			typ, reason = domain.EvItemPhaseChanged, st.Reason
			pl = payload{Op: "transition", Before: &before, Item: cur, Precision: st.Precision}
		case StepProgress:
			nprog++
			p := domain.Progress{ID: fmt.Sprintf("%s.p%06d", id, nprog), Text: st.Text, On: st.On, Actor: c.Caller.Actor.Principal, At: st.At.UTC(), Precision: st.Precision}
			if err := insertProgress(c.Tx, id, p); err != nil {
				return "", err
			}
			cur = stamped(cur, st.At)
			typ = domain.EvItemProgressAdded
			pl = payload{Op: "progress", Item: cur, Progress: &p, Precision: st.Precision}
		default:
			return "", invalid("unknown import step " + st.Event)
		}
		b, err := marshalPayload(pl)
		if err != nil {
			return "", err
		}
		if _, err := c.Emit(domain.Event{Type: typ, Object: id, Reason: reason, Payload: b, At: st.At, Imported: info.Batch, RecordedAt: info.At}); err != nil {
			return "", err
		}
		if _, err := command.Bump(c.Tx, id); err != nil {
			return "", err
		}
	}
	if err := save(c.Tx, cur); err != nil {
		return "", err
	}
	return id, syncTimer(c.Tx, domain.Item{}, cur)
}

// ---- 导入批次与对照 ----

// ImportBatch 是一个导入批次：同一来源名、同一调用者的若干次请求共用；Count 是已新建的条数。
type ImportBatch struct {
	ID, Source, Actor, Grant string
	Count                    int
	At                       time.Time
}

// ImportMapGet 按（来源名，记录类型，外部 id）找已导入的账本对象 id。
func ImportMapGet(tx *store.Tx, source, recordType, externalID string) (string, bool, error) {
	r, err := tx.Query(`SELECT object_id FROM item_import_map WHERE source = ? AND record_type = ? AND external_id = ?`, source, recordType, externalID)
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	if !r.Next() {
		return "", false, r.Err()
	}
	var id string
	err = r.Scan(&id)
	return id, err == nil, err
}

// ImportMapPut 登记对照；已有同键对照是调用方的错（幂等判断在写入之前）。
func ImportMapPut(tx *store.Tx, source, recordType, externalID, object string) error {
	_, err := tx.Exec(`INSERT INTO item_import_map (source, record_type, external_id, object_id) VALUES (?, ?, ?, ?)`, source, recordType, externalID, object)
	return err
}

// ImportBatchEnsure 登记批次（已有则不动）；返回的批次是账本里的那一行。
func ImportBatchEnsure(tx *store.Tx, b ImportBatch) (ImportBatch, error) {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO item_import_batch (id, source, actor, grant_id, count, at) VALUES (?, ?, ?, ?, 0, ?)`,
		b.ID, b.Source, b.Actor, b.Grant, b.At.UnixNano()); err != nil {
		return b, err
	}
	got, _, err := ImportBatchGet(tx, b.ID)
	return got, err
}

// ImportBatchAdd 把批次已新建的条数加 n。
func ImportBatchAdd(tx *store.Tx, id string, n int) error {
	_, err := tx.Exec(`UPDATE item_import_batch SET count = count + ? WHERE id = ?`, n, id)
	return err
}

const batchCols = "id, source, actor, grant_id, count, at"

func scanBatches(tx *store.Tx, where string, args ...any) ([]ImportBatch, error) {
	r, err := tx.Query("SELECT "+batchCols+" FROM item_import_batch "+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []ImportBatch
	for r.Next() {
		var b ImportBatch
		var count, at int64
		if err := r.Scan(&b.ID, &b.Source, &b.Actor, &b.Grant, &count, &at); err != nil {
			return nil, err
		}
		b.Count = int(count)
		b.At = time.Unix(0, at).UTC()
		out = append(out, b)
	}
	return out, r.Err()
}

// ImportBatchGet 读一个批次。
func ImportBatchGet(tx *store.Tx, id string) (ImportBatch, bool, error) {
	bs, err := scanBatches(tx, "WHERE id = ?", id)
	if err != nil || len(bs) == 0 {
		return ImportBatch{}, false, err
	}
	return bs[0], true, nil
}

// ImportBatches 返回全部批次，最新在前。
func ImportBatches(tx *store.Tx) ([]ImportBatch, error) {
	return scanBatches(tx, "ORDER BY at DESC, id DESC")
}
