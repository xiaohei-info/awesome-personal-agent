// Package purge 是物理删除（设计 §6.15）：本人在 Web 上彻底删除一个事项或材料的内容。
// purge.object 在一个事务内删除对象的投影与正文，把该对象全部 Event 的内容字段替换为墓碑
// （序号、类型、对象 id、时间、actor 保留，所以序号与订阅游标不受影响），另记一条删除事件；
// 正文引用的 blob 在没有其他材料引用时才删除，有引用的保留。墓碑不计入 Event 统计（统计方读到 Tombstone 即跳过）。
//
// 授权是 change_settings：只有 Web 上的本人可执行，delegate 得到退出码 20，其余级别被拒绝。
// 本包不写自己的表，也不直写其他模块的数据：投影、正文和 blob 文件的删除分别由 items、materials、blobs 提供入口，
// Event 墓碑由 eventlog 提供。
package purge

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Command 是登记的命令名。
const Command = "purge.object"

// EvPurged 是删除事件的类型（内部记录，`apa why` 可见）；载荷只有统计与 blob 哈希，不含被删内容。
const EvPurged = "purge.executed"

// 被删对象的种类。
const (
	KindItem     = "item"
	KindMaterial = "material"
)

// Result 是 purge.object 的结果。
type Result struct {
	Object        string   `json:"object"`
	Kind          string   `json:"kind"`
	Events        int64    `json:"events"`                   // 新墓碑化的事件条数
	BlobsRemoved  []string `json:"blobs_removed,omitempty"`  // 已删除的 blob 哈希
	BlobsRetained []string `json:"blobs_retained,omitempty"` // 仍被其他材料引用而保留的 blob 哈希
}

type in struct {
	Object string `json:"object"`
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
		Name: Command, Action: authz.ActionChangeSettings,
		Object: func(_ *store.Tx, raw json.RawMessage) (authz.Object, error) {
			v, err := decode(raw)
			return authz.Object{ID: v.Object}, err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := decode(c.Input)
			if err != nil {
				return nil, err
			}
			return apply(c, blobDir, v.Object)
		},
	})
}

func apply(c *command.Ctx, blobDir, id string) (any, error) {
	var (
		kind string
		refs []string
	)
	_, err := items.Get(c.Tx, id)
	switch {
	case err == nil:
		kind = KindItem
		if err := items.Purge(c.Tx, id); err != nil {
			return nil, err
		}
		// 判断记录只含结构化摘要，但指向被删对象的一并删除（§6.16）。
		if _, err := record.DeleteBySubject(c.Tx, id); err != nil {
			return nil, err
		}
	case isNotFound(err):
		if refs, err = materials.Purge(c.Tx, id); err != nil {
			if isNotFound(err) {
				return nil, domain.NewError(domain.CodeNotFound, "object "+id+" is neither an item nor a material", "list items or materials and retry")
			}
			return nil, err
		}
		kind = KindMaterial
	default:
		return nil, err
	}

	n, err := eventlog.Tombstone(c.Tx, id)
	if err != nil {
		return nil, err
	}
	res := Result{Object: id, Kind: kind, Events: n}
	var drop []string
	for _, ref := range refs {
		h, err := blobs.ParseRef(ref)
		if err != nil {
			return nil, err
		}
		used, err := materials.BlobReferenced(c.Tx, ref)
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
