package inbound

import (
	"cmp"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 建议的上游对象与"已结束"的依据（§7.2 建议流的分组与按上游已结束批量忽略） ----
//
// 上游对象由来源事件的链接认出：链接去掉协议、主机、查询与片段后的路径（同一个 PR 的评论链接带 #片段，归同一对象）。
// 带对象生命周期事件（类型以 .opened、.merged、.closed 结尾，来源执行契约的类型约定 <对象>.<动作>）的才算上游对象，
// 只按结构化字段判断，不读标题与正文：
//   - 建议属于对象：它的某条来源事件的链接就是这个对象；或某条来源事件的关联键（去掉 "<种类>:" 前缀）以对象路径开头、
//     且紧接的不是数字（别的来源转来的同一对象的通知，例如线程号以对象路径开头的邮件）。都对不上的不归任何对象。
//   - 对象已结束：有 .merged 事件（合并不可逆）；或有 .closed 事件且它之后该对象再没有别的事件（关闭后重开表现为更新，有后续就拿不准）。

// ObjectEnded 是对象已结束的依据：哪条来源事件、什么类型、何时发生。
type ObjectEnded struct {
	Event string    `json:"event"`
	Type  string    `json:"type"`
	At    time.Time `json:"at"`
}

// Origin 是一条建议在建议流上分组所需的来源信息：第一条来源事件的类型、所属上游对象、对象已结束的依据。
type Origin struct {
	Type   string       `json:"type,omitempty"`
	Object string       `json:"object,omitempty"`
	Ended  *ObjectEnded `json:"ended,omitempty"`
}

type objEvent struct {
	id, typ string
	at      time.Time
	seq     int64
}

func lifecycle(typ string) bool {
	return strings.HasSuffix(typ, ".opened") || strings.HasSuffix(typ, ".merged") || strings.HasSuffix(typ, ".closed")
}

// objectPath 是链接的对象路径；不是 http(s) 链接时为空。
func objectPath(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

func ended(es []objEvent) *ObjectEnded {
	for _, e := range es {
		if strings.HasSuffix(e.typ, ".merged") {
			return &ObjectEnded{e.id, e.typ, e.at}
		}
	}
	for i, e := range es {
		if strings.HasSuffix(e.typ, ".closed") && !slices.ContainsFunc(es[i+1:], func(x objEvent) bool { return !strings.HasSuffix(x.typ, ".closed") }) {
			return &ObjectEnded{e.id, e.typ, e.at}
		}
	}
	return nil
}

// Origins 给出各条建议的 Origin（没有来源事件的建议不在结果里）。只读。
// shortcut: 每次读全部带链接的来源事件在内存里配对，事件到数万条时改为按对象路径建索引列。
func Origins(tx *store.Tx, props []domain.Item) (map[string]Origin, error) {
	rows, err := tx.Query(`SELECT id, type, link, occurred_at, seq, correlation_keys FROM sources_runner_event`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type evRow struct {
		typ, path string
		keys      []string
	}
	byID := map[string]evRow{}
	objs := map[string][]objEvent{}
	for rows.Next() {
		var (
			id, typ, link, keys string
			at, seq             int64
		)
		if err := rows.Scan(&id, &typ, &link, &at, &seq, &keys); err != nil {
			return nil, err
		}
		r := evRow{typ: typ, path: objectPath(link)}
		_ = json.Unmarshal([]byte(keys), &r.keys) // 关联键是入账时校验过的 JSON 数组；读不出按没有
		byID[id] = r
		if r.path != "" {
			objs[r.path] = append(objs[r.path], objEvent{id, typ, time.Unix(0, at).UTC(), seq})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for p, es := range objs {
		if !slices.ContainsFunc(es, func(e objEvent) bool { return lifecycle(e.typ) }) {
			delete(objs, p)
			continue
		}
		slices.SortFunc(es, func(a, b objEvent) int { return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.seq, b.seq)) })
	}
	byKey := func(k string) string {
		if i := strings.Index(k, ":"); i >= 0 {
			k = k[i+1:]
		}
		best := ""
		for p := range objs {
			if len(p) > len(best) && strings.HasPrefix(k, p) && (len(k) == len(p) || k[len(p)] < '0' || k[len(p)] > '9') {
				best = p
			}
		}
		return best
	}
	out := map[string]Origin{}
	for _, it := range props {
		if it.Proposal == nil || len(it.Proposal.Sources) == 0 {
			continue
		}
		var o Origin
		for i, sid := range it.Proposal.Sources {
			r, ok := byID[sid]
			if !ok {
				continue
			}
			if i == 0 {
				o.Type = r.typ
			}
			if o.Object == "" && objs[r.path] != nil {
				o.Object = r.path
			}
			for _, k := range r.keys {
				if o.Object == "" {
					o.Object = byKey(k)
				}
			}
		}
		if o.Object != "" {
			o.Ended = ended(objs[o.Object])
		}
		out[it.ID] = o
	}
	return out, nil
}
