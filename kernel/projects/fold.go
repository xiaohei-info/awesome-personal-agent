package projects

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// fold 按序读完事件日志，折叠出项目：project.updated 带变更后的完整快照，
// allowed_executors_changed 只带名单（after），叠在当前快照上。已墓碑化的项目视为物理删除。
func fold(tx *store.Tx) (map[string]domain.Project, error) {
	out := map[string]domain.Project{}
	var after int64
	for {
		es, err := eventlog.Read(tx, after, 500)
		if err != nil {
			return nil, err
		}
		if len(es) == 0 {
			return out, nil
		}
		for _, en := range es {
			after = en.Event.Seq
			if en.Tombstone {
				delete(out, en.Event.Object)
				continue
			}
			switch en.Event.Type {
			case domain.EvProjectUpdated:
				var p struct {
					Project domain.Project `json:"project"`
				}
				if err := json.Unmarshal(en.Event.Payload, &p); err != nil || p.Project.ID != en.Event.Object {
					continue
				}
				out[p.Project.ID] = p.Project
			case domain.EvProjectAllowedExecutorsChanged:
				var p struct {
					After []string `json:"after"`
				}
				cur, ok := out[en.Event.Object]
				if err := json.Unmarshal(en.Event.Payload, &p); err != nil || !ok {
					continue
				}
				cur.AllowedExecutors = p.After
				out[cur.ID] = cur
			}
		}
	}
}

// Verify 比较项目投影与事件折叠的结果，不一致返回描述第一处差异的错误。
func Verify(tx *store.Tx) error {
	want, err := fold(tx)
	if err != nil {
		return err
	}
	have, err := List(tx)
	if err != nil {
		return err
	}
	if len(have) != len(want) {
		return fmt.Errorf("projects: projection has %d projects, events fold to %d", len(have), len(want))
	}
	for _, p := range have {
		w, ok := want[p.ID]
		if !ok {
			return fmt.Errorf("projects: project %s is not in the event log", p.ID)
		}
		if !reflect.DeepEqual(canon(p), canon(w)) {
			return fmt.Errorf("projects: project %s differs from its event fold", p.ID)
		}
	}
	return nil
}

// canon 经 JSON 往返抹平 nil 与空切片等无语义差异。
func canon(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
