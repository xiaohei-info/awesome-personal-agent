// Package purge 是物理删除（设计 §6.15）：本人在 Web 上彻底删除一个事项或材料的内容。
// purge.object 在一个事务内删除对象的投影与正文，把该对象全部 Event 的内容字段替换为墓碑
// （序号、类型、对象 id、时间、actor 保留，所以序号与订阅游标不受影响），另记一条删除事件；
// 正文引用的 blob 在没有其他材料引用时才删除，有引用的保留。墓碑不计入 Event 统计（统计方读到 Tombstone 即跳过）。
//
// 授权是 change_settings：只有 Web 上的本人可执行，delegate 得到退出码 20，其余级别被拒绝。
// 本包不写自己的表，也不直写其他模块的数据：投影、正文、委派记录和 blob 文件的删除分别由 items、materials、delegation、blobs 提供入口，
// Event 墓碑由 eventlog 提供，检索索引的清除由 search 提供。
package purge

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Command 是登记的命令名。
const Command = "purge.object"

// EvPurged 是删除事件的类型（内部记录，`apa why` 可见）；载荷只有统计与 blob 哈希，不含被删内容。
const EvPurged = "purge.executed"

// 被删对象的种类。
const (
	KindItem        = "item"
	KindMaterial    = "material"
	KindSourceEvent = "source_event"
)

// Result 是 purge.object 的结果。
type Result struct {
	Object        string   `json:"object"`
	Kind          string   `json:"kind"`
	Events        int64    `json:"events"`                   // 新墓碑化的事件条数
	SourceEvents  []string `json:"source_events,omitempty"`  // 一并删除的来源事件 id（对象是来源事件时为空，对象本身即是）
	BlobsRemoved  []string `json:"blobs_removed,omitempty"`  // 已删除的 blob 哈希
	BlobsRetained []string `json:"blobs_retained,omitempty"` // 仍被其他材料引用而保留的 blob 哈希
}

type in struct {
	Object string `json:"object"`
	// WithSourceEvents 只用于事项：连同挂在它上面的来源事件一起删除（§6.15）。
	WithSourceEvents bool `json:"with_source_events,omitempty"`
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func decode(raw json.RawMessage) (in, error) {
	var v in
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, invalid("input must be {\"object\": <item or material id>}")
	}
	if v.Object == "" {
		return v, invalid("object is empty")
	}
	return v, nil
}

// Register 在 x 上登记 purge.object；blobDir 是 <APA_HOME>/blobs。
func Register(x *command.Executor, blobDir string) error {
	return x.Register(command.Spec{
		Name: Command, Action: authz.ActionChangeSettings, Group: authz.GroupPurge, Page: "/purge", // 物理删除只由本人做（裁决 3）
		Object: func(_ *store.Tx, raw json.RawMessage) (authz.Object, error) {
			v, err := decode(raw)
			return authz.Object{ID: v.Object}, err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := decode(c.Input)
			if err != nil {
				return nil, err
			}
			return apply(c, blobDir, v)
		},
	})
}

func apply(c *command.Ctx, blobDir string, v in) (any, error) {
	id := v.Object
	var (
		kind    string
		refs    []string // 删除对象带走的 blob 引用（apa://blob/<hash>）
		deleted []string // 一并删除的来源事件 id
	)
	_, err := items.Get(c.Tx, id)
	switch {
	case err == nil:
		kind = KindItem
		if v.WithSourceEvents {
			if deleted, err = runner.EventsOfItem(c.Tx, id); err != nil {
				return nil, err
			}
		}
		if err := items.Purge(c.Tx, id); err != nil {
			return nil, err
		}
		if err := delegation.Purge(c.Tx, id); err != nil {
			return nil, err
		}
		// 判断记录只含结构化摘要，但指向被删对象的一并删除（§6.16）。
		if _, err := record.DeleteBySubject(c.Tx, id); err != nil {
			return nil, err
		}
	case isNotFound(err):
		if v.WithSourceEvents {
			return nil, invalid("with_source_events only applies to an item")
		}
		if _, err := runner.Event(c.Tx, id); err == nil {
			kind, deleted = KindSourceEvent, []string{id}
		} else if !isNotFound(err) {
			return nil, err
		} else if refs, err = materials.Purge(c.Tx, id); err != nil {
			if isNotFound(err) {
				return nil, domain.NewError(domain.CodeNotFound, "object "+id+" is neither an item, a material nor a source event", "list items or materials and retry")
			}
			return nil, err
		} else {
			kind = KindMaterial
		}
	default:
		return nil, err
	}

	// 来源事件：事件行、原文与指向它的判断记录都删，去重键留作墓碑；相关 Event 与其他对象一样墓碑化。
	for _, ev := range deleted {
		hs, err := runner.PurgeEvent(c.Tx, ev)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			refs = append(refs, blobs.Ref(h))
		}
		if _, err := record.DeleteBySubject(c.Tx, ev); err != nil {
			return nil, err
		}
	}
	tomb := append([]string{id}, deleted...) // 来源事件对象时 id 重复一次，墓碑化幂等

	n, err := eventlog.Tombstone(c.Tx, tomb...)
	if err != nil {
		return nil, err
	}
	// 检索索引是派生数据，被删对象不会再有新事件：在同一事务里清掉，删完立即检索不到（§6.15）。
	if err := search.Forget(c.Tx, tomb...); err != nil {
		return nil, err
	}
	res := Result{Object: id, Kind: kind, Events: n}
	slices.Sort(refs)
	refs = slices.Compact(refs) // 几封事件可能取回同一个附件
	if kind == KindItem {
		res.SourceEvents = deleted
	}
	var drop []string
	for _, ref := range refs {
		h, err := blobs.ParseRef(ref)
		if err != nil {
			return nil, err
		}
		used, err := materials.BlobReferenced(c.Tx, ref)
		if err == nil && !used {
			used, err = runner.BlobReferenced(c.Tx, h)
		}
		if err != nil {
			return nil, err
		}
		if used {
			res.BlobsRetained = append(res.BlobsRetained, h)
		} else {
			res.BlobsRemoved = append(res.BlobsRemoved, h)
			drop = append(drop, h)
		}
	}
	slices.Sort(res.BlobsRemoved)
	slices.Sort(res.BlobsRetained)
	payload, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvPurged, Object: id, Payload: payload}); err != nil {
		return nil, err
	}
	// 文件删除放在最后：之前任何一步失败都会回滚事务且不丢 blob。
	// shortcut: 文件删除后若提交失败，blob 已不可恢复；purge 本就不可逆，重试同一命令即可补完，量大时再加提交后钩子。
	for _, h := range drop {
		if err := blobs.Remove(blobDir, h); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "blob storage failed", "check the data directory and retry")
		}
	}
	return res, nil
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}
