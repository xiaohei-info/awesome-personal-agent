package inbound

import (
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// oldBuild 把账本改回线上旧版本留下的样子：evs 是挂在 from 名下的来源对象更新，旧版没记进 from 的消息清单；
// to 非空时 from 已被归类折进 to，旧版折叠只搬了清单里的，evs 留在 from 名下、to 上没有它们。照常写事项事件，事件日志仍能重建。
func (e *env) oldBuild(from, to string, evs []string) {
	e.t.Helper()
	strip := func(tx *store.Tx, id string) error {
		it, err := items.Get(tx, id)
		if err != nil {
			return err
		}
		p := *it.Proposal
		p.Sources = slices.DeleteFunc(slices.Clone(p.Sources), func(s string) bool { return slices.Contains(evs, s) })
		p.Merges = slices.DeleteFunc(slices.Clone(p.Merges), func(m domain.ProposalMerge) bool { return slices.Contains(evs, m.Event) })
		w, err := items.SetProposal(tx, id, p, e.now)
		if err != nil {
			return err
		}
		if _, err := command.Bump(tx, id); err != nil {
			return err
		}
		_, err = eventlog.Append(tx, domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, At: e.now, Reason: "as the old build left it",
			Actor: domain.Actor{Type: domain.ActorSystem, Principal: systemPrinc, Via: domain.ViaSource}})
		return err
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if to != "" {
			if err := strip(tx, to); err != nil {
				return err
			}
			for _, ev := range evs {
				if err := runner.Repoint(tx, ev, to, from); err != nil {
					return err
				}
			}
		}
		return strip(tx, from)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// legacyFold 按旧版的样子落一次归类：to 留着，其余逐条 merge 进 to（直接发 inbound.sweep，没有整合）。
func (e *env) legacyFold(to string, from ...string) {
	e.t.Helper()
	if err := e.svc.settle(bg, system, sweepIn{Item: to, Batch: "mb_legacy", Action: domain.SweepKeep, By: "generate", Why: "留着"}); err != nil {
		e.t.Fatal(err)
	}
	for _, id := range from {
		if err := e.svc.settle(bg, system, sweepIn{Item: id, Batch: "mb_legacy", Action: domain.SweepMerge, Target: to, By: "generate", Why: "同一个 PR #227"}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) lastSeq() (n int64) {
	e.view(func(tx *store.Tx) {
		r, err := tx.Query(`SELECT COALESCE(MAX(seq), 0) FROM eventlog_event`)
		if err != nil {
			e.t.Fatal(err)
		}
		defer r.Close()
		if r.Next() {
			_ = r.Scan(&n)
		}
	})
	return
}

// 一次性修复（apa repair-merges）：线上 PR #227 折叠后，平台上的三条更新留在被丢弃的 gp 名下、没进留下的 keep；
// 另有一条仍是建议的平台 PR，挂着的更新没进它的消息清单。修复把前者搬到 keep（按先后，每条记从 gp 来，分数、理由同那次折叠），
// 把后者补记进自己的清单；第二遍零变更；修复后撤销合并照样整组搬回。
func TestRepairMovesWhatOldFoldsLeftBehind(t *testing.T) {
	e := followEnv(t)
	keep, mid, gp, i, m, g := pr227(e)
	e.emit("src_github", "pr", ev("h1", "pr.opened", "fix: 设置页保存按钮", map[string]any{"sender": "dev", "link": "https://github.com/org/app/pull/228", "correlation_keys": []string{"org/app#228"}}),
		ev("h2", "pr.updated", "fix: 设置页保存按钮", map[string]any{"sender": "dev", "link": "https://github.com/org/app/pull/228", "correlation_keys": []string{"org/app#228"}}))
	other := e.byTitle("fix: 设置页保存按钮")
	h := e.sev("src_github", "h1", "h2")
	e.legacyFold(keep.ID, mid.ID, gp.ID) // 旧版那次归类：mid、gp 并进 keep（没有整合）
	e.oldBuild(gp.ID, keep.ID, g[1:])
	e.oldBuild(other.ID, "", h[1:])
	all := append(append(append([]string{}, i...), m...), g[0])
	if got := e.item(keep.ID).Proposal; !slices.Equal(got.Sources, all) || len(got.Merges) != 4 || e.events("src_github")[1].Item != gp.ID {
		t.Fatalf("the live shape before the repair: %+v", got)
	}
	e.verify()

	done, err := Repair(bg, e.x)
	if err != nil {
		t.Fatal(err)
	}
	want := []Repaired{{Item: gp.ID, Title: gp.Title, Into: keep.ID, Events: g[1:]}, {Item: other.ID, Title: other.Title, Into: other.ID, Events: h[1:]}}
	if len(done) != 2 || !slices.EqualFunc(done, want, func(a, b Repaired) bool {
		return a.Item == b.Item && a.Title == b.Title && a.Into == b.Into && slices.Equal(a.Events, b.Events)
	}) {
		t.Fatalf("repaired %+v, want %+v", done, want)
	}
	got := e.item(keep.ID).Proposal
	if !slices.Equal(got.Sources, append(all, g[1:]...)) || len(got.Merges) != 7 {
		t.Fatalf("keep lists all 9 in arrival order: %+v", got)
	}
	first := got.Merges[3] // 折叠时搬过来的 g1
	for k, x := range got.Merges[4:] {
		if x.Event != g[k+1] || x.Item != gp.ID || x.Source != "src_github" || x.By != first.By || x.Score != first.Score || x.Why != first.Why || x.Batch != first.Batch || x.Run != first.Run {
			t.Fatalf("repaired trace %d: %+v, fold trace %+v", k, x, first)
		}
	}
	for _, x := range e.events("src_github")[:4] {
		if x.Item != keep.ID {
			t.Fatalf("left behind: %+v", x)
		}
	}
	if p := e.item(other.ID).Proposal; !slices.Equal(p.Sources, h) || len(p.Merges) != 0 {
		t.Fatalf("the proposal lists its update: %+v", p)
	}
	e.verify()

	seq, versions := e.lastSeq(), map[string]int64{}
	for _, it := range e.allItems() {
		versions[it.ID] = it.Version
	}
	again, err := Repair(bg, e.x)
	if err != nil || len(again) != 0 || e.lastSeq() != seq {
		t.Fatalf("second run must change nothing: %+v %v, seq %d -> %d", again, err, seq, e.lastSeq())
	}
	for _, it := range e.allItems() {
		if it.Version != versions[it.ID] {
			t.Fatalf("%s version %d -> %d", it.ID, versions[it.ID], it.Version)
		}
	}

	if err := e.svc.Unmerge(bg, owner, gp.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.item(keep.ID).Proposal; !slices.Equal(got.Sources, all[:4]) || len(got.Merges) != 3 {
		t.Fatalf("unmerge after the repair takes all four back: %+v", got)
	}
	for _, x := range e.events("src_github")[:4] {
		if x.Item != gp.ID {
			t.Fatalf("back on gp: %+v", x)
		}
	}
	e.verify()
}
