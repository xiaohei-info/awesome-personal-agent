//go:build e2e

package proposals

import (
	"encoding/json"
	"slices"
	"testing"
)

const srcG = "src_accpgb"

type group struct {
	Key     string     `json:"key"`
	Kind    string     `json:"kind"`
	Topic   string     `json:"topic"`
	Items   []string   `json:"items"`
	Bundles [][]string `json:"bundles"`
}

type bulkResult struct {
	ID        string `json:"id"`
	Do        string `json:"do"`
	OK        bool   `json:"ok"`
	Skip      string `json:"skip"`
	Into      string `json:"into"`
	Batch     string `json:"batch"`
	UndoUntil string `json:"undo_until"`
}

func (k *kit) groups() ([]group, []string) {
	k.t.Helper()
	var out struct {
		Items  []struct{ ID string } `json:"items"`
		Groups []group               `json:"groups"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/items?phase=proposed", "", 200)), &out))
	var ids []string
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	return out.Groups, ids
}

func (k *kit) bulk(body, key string) map[string]bulkResult {
	k.t.Helper()
	r := k.WebWith("POST", "/v1/proposals/batch", body, map[string]string{"Idempotency-Key": key})
	if r.Code != 200 {
		k.t.Fatalf("POST /proposals/batch: HTTP %d %s", r.Code, r.Body)
	}
	var out struct {
		Results []bulkResult `json:"results"`
	}
	k.must(json.Unmarshal([]byte(r.Body), &out))
	m := map[string]bulkResult{}
	for _, x := range out.Results {
		m[x.ID] = x
	}
	return m
}

func groupOf(gs []group, id string) (group, int) {
	n, at := 0, group{}
	for _, g := range gs {
		if slices.Contains(g.Items, id) {
			n++
			at = g
		}
	}
	return at, n
}

// TestFlowProposalGroupBulk：建议页按主题现算分组（每条恰好一组，合并键相同的同组）；按选中批量接受时同一对象合成一个事项、
// 不同对象各建一个；同一批重发不重复处理；一次批量一条审计事件；按批撤销后建议回到原状态；批量忽略带原因、按批列出。
func TestFlowProposalGroupBulk(t *testing.T) {
	k := newKit(t)
	var ci1, ci2, ob1, ob2, batch string

	k.step("ACT-PGB-01 两条 CI 失败通知、两条指向同一 PR 的评审请求进建议流", func(t *testing.T) {
		k.registerSource(srcG)
		k.screen("pgb", 0.90)
		k.ModelStub.Judge(judgeYes("pgb"))
		k.emit(srcG, Ev{ID: "pgb-ci-1", Type: "mail.received", Subject: "pgb 修复 main 分支 CI build 失败（一）"})
		k.emit(srcG, Ev{ID: "pgb-ci-2", Type: "mail.received", Subject: "pgb 排查 main 分支 CI e2e 失败（二）"})
		k.emit(srcG, Ev{ID: "pgb-ob-1", Type: "mail.received", Subject: "pgb 评审 PR #77：新增导出", Link: "https://code.test/o/r/pull/77"})
		ci1, ci2, ob1 = k.proposalFrom(srcG, "pgb-ci-1"), k.proposalFrom(srcG, "pgb-ci-2"), k.proposalFrom(srcG, "pgb-ob-1")
		// 线上同一对象两条建议的来历：第一条被忽略后同一对象又来了消息另起一条，之后第一条被改回建议。
		k.webOK("POST", "/items/"+ob1+"/dismiss", `{}`, 200)
		k.emit(srcG, Ev{ID: "pgb-ob-2", Type: "mail.received", Subject: "pgb PR #77 请求评审（再次提醒）", Link: "https://code.test/o/r/pull/77"})
		ob2 = k.proposalFrom(srcG, "pgb-ob-2")
		k.webOK("POST", "/items/"+ob1+"/transition", `{"to":"proposed","reason":"改回建议"}`, 200)
		if ob1 == ob2 || k.proposalOf(ob1)["merge_key"] != k.proposalOf(ob2)["merge_key"] || k.proposalOf(ob1)["merge_key"] == nil {
			t.Fatalf("应有两条合并键相同的建议: %v %v", k.proposalOf(ob1), k.proposalOf(ob2))
		}
	})

	k.step("ACT-PGB-02 分组：每条恰好一组，合并键相同的同组，组内按时间", func(t *testing.T) {
		gs, ids := k.groups()
		for _, id := range ids {
			if _, n := groupOf(gs, id); n != 1 {
				t.Errorf("%s 出现在 %d 个组里", id, n)
			}
		}
		g, _ := groupOf(gs, ci1)
		eq(t, "CI 失败的组", g.Key, "topic:ci_failure")
		if !slices.Equal(g.Items, []string{ci1, ci2}) {
			t.Errorf("CI 失败组按时间: %v", g.Items)
		}
		g, _ = groupOf(gs, ob1)
		eq(t, "评审请求的组", g.Key, "topic:review_request")
		if h, _ := groupOf(gs, ob2); h.Key != g.Key || len(g.Bundles) != 1 || !slices.Equal(g.Bundles[0], []string{ob1, ob2}) {
			t.Errorf("同一对象的两条应同组、成一份: %+v", g)
		}
	})

	k.step("ACT-PGB-03 按选中批量接受：同一对象合成一个事项，不同对象各建一个；重发不重复处理", func(t *testing.T) {
		body := `{"mode":"selected","do":"accept","filter":{"ids":["` + ob1 + `","` + ob2 + `","` + ci1 + `"]}}`
		res := k.bulk(body, "pgb-accept-1")
		if !res[ob1].OK || res[ob1].Do != "accept" || !res[ob2].OK || res[ob2].Do != "merge" || res[ob2].Into != ob1 || !res[ci1].OK || res[ci1].UndoUntil == "" {
			t.Fatalf("结果: %+v", res)
		}
		batch = res[ob1].Batch
		eq(t, "ob1", k.phase(ob1), "open")
		eq(t, "ob2", k.phase(ob2), "dropped")
		eq(t, "ci1", k.phase(ci1), "open")
		eq(t, "ci2 没选不动", k.phase(ci2), "proposed")
		if src := k.sourcesOf(ob1); !slices.Contains(src, k.sevID(srcG, "pgb-ob-2")) {
			t.Errorf("ob2 的消息应搬到 ob1: %v", src)
		}
		again := k.bulk(body, "pgb-accept-1")
		for _, id := range []string{ob1, ob2, ci1} {
			if again[id].OK || again[id].Skip != "handled_here" {
				t.Errorf("重发 %s: %+v", id, again[id])
			}
		}
		es := k.events(batch, "proposal.batch_handled")
		if len(es) != 1 || es[0].Payload["requested"] != float64(3) || es[0].Payload["do"] != "accept" {
			t.Fatalf("一次批量一条审计事件: %+v", es)
		}
	})

	k.step("ACT-PGB-04 按批撤销：三条回到建议，合进去的消息搬回", func(t *testing.T) {
		res := k.bulk(`{"mode":"undo","batch":"`+batch+`"}`, "pgb-undo-1")
		for _, id := range []string{ob1, ob2, ci1} {
			if !res[id].OK {
				t.Errorf("撤销 %s: %+v", id, res[id])
			}
			eq(t, id+" 撤销后", k.phase(id), "proposed")
		}
		if src := k.sourcesOf(ob1); slices.Contains(src, k.sevID(srcG, "pgb-ob-2")) {
			t.Errorf("撤销后 ob2 的消息应回到 ob2: %v", src)
		}
		eq(t, "撤销的审计事件", len(k.events(batch, "proposal.batch_undone")), 1)
		gs, _ := k.groups()
		if g, _ := groupOf(gs, ob2); g.Key != "topic:review_request" {
			t.Errorf("撤销后回到原来的组: %+v", g)
		}
	})

	k.step("ACT-PGB-05 按组批量忽略带原因，已忽略里按批列出", func(t *testing.T) {
		res := k.bulk(`{"mode":"selected","do":"dismiss","reason":"CI 已修好","filter":{"ids":["`+ci1+`","`+ci2+`"]}}`, "pgb-dismiss-1")
		if !res[ci1].OK || !res[ci2].OK {
			t.Fatalf("忽略: %+v", res)
		}
		eq(t, "ci2", k.phase(ci2), "dropped")
		eq(t, "忽略原因", k.last(ci2, "item.phase_changed").Reason, "CI 已修好")
		var out struct {
			Dismissals map[string]struct {
				Batch  string `json:"batch"`
				Reason string `json:"reason"`
			} `json:"dismissals"`
		}
		k.must(json.Unmarshal([]byte(k.webOK("GET", "/items?phase=dropped", "", 200)), &out))
		if d := out.Dismissals[ci2]; d.Batch != res[ci2].Batch || d.Reason != "CI 已修好" {
			t.Errorf("已忽略按批: %+v", d)
		}
	})
}
