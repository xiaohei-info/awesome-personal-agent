package runner

import (
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// SetOutcome 在调用方事务内记下一条来源事件处置的附带结果：最近一次判断记录 id，以及一次生成多个事项（extract 出口）时的全部事项 id。
// 空值不覆盖已有内容；与 SetDisposition 同事务调用。
func SetOutcome(tx *store.Tx, id, judgment string, items []string) error {
	if judgment != "" {
		if _, err := tx.Exec(`UPDATE sources_runner_event SET judgment = ? WHERE id = ?`, judgment, id); err != nil {
			return err
		}
	}
	if len(items) > 0 {
		b, err := json.Marshal(items)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sources_runner_event SET items = ? WHERE id = ?`, string(b), id); err != nil {
			return err
		}
	}
	return nil
}
