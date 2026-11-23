package health

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) deliveries() (n int, ids []string) {
	e.t.Helper()
	e.update(func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT id FROM delivery_delivery ORDER BY id`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id string
			if err := r.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return r.Err()
	})
	return len(ids), ids
}

func (e *env) push(p *Pusher) {
	e.t.Helper()
	if err := p.Sync(bg, e.st); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setPush(cat string, r policy.PushRule) {
	e.t.Helper()
	e.update(func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "push", func(p *policy.Policy) error {
			if p.Disturb.Push == nil {
				p.Disturb.Push = map[string]policy.PushRule{}
			}
			p.Disturb.Push[cat] = r
			return nil
		})
		return err
	})
}

// 来源连续失败：一串失败只推一次（连续失败第 3 次与随后的认证失败是同一串）；启动前的旧事件不补推；关掉这一类就不推。
func TestPushSourceFailingOncePerStreak(t *testing.T) {
	e := setup(t)
	last := t0.Add(-time.Hour)
	e.source("src_x", true, runner.Stats{ConsecutiveFailures: 3, LastSuccessAt: last})
	e.event(domain.EvSourceFailed, "src_x", "", map[string]any{"kind": "streak"}) // 启动之前的事件
	p := NewPusher(e.att())
	e.push(p)
	if n, _ := e.deliveries(); n != 0 {
		t.Fatal("events before start must not be pushed")
	}
	e.event(domain.EvSourceFailed, "src_x", "", map[string]any{"kind": "streak"})
	e.event(domain.EvSourceFailed, "src_x", "", map[string]any{"kind": "auth_or_config"})
	e.push(p)
	n, ids := e.deliveries()
	if want := fmt.Sprintf("push:source:src_x:%d:fail", last.Unix()); n != 1 || ids[0] != want {
		t.Fatalf("want one push %s, got %v", want, ids)
	}

	e.setPush(policy.PushSourceFailing, policy.PushRule{On: false})
	e.source("src_y", true, runner.Stats{ConsecutiveFailures: 3})
	e.event(domain.EvSourceFailed, "src_y", "", map[string]any{"kind": "streak"})
	e.push(p)
	if n, _ := e.deliveries(); n != 1 {
		t.Fatal("a category switched off must not push")
	}
}

// 低频来源失败一次就推（runner.AlertNow），文字说明为什么不等连续三次；同一串的下一次失败不再推第二条。
func TestPushLowFrequencySourceOnFirstFailure(t *testing.T) {
	e := setup(t)
	last := t0.Add(-time.Hour)
	p := NewPusher(e.att())
	e.push(p)
	e.source("src_dx", true, runner.Stats{ConsecutiveFailures: 1, LastSuccessAt: last})
	e.event(domain.EvSourceFailed, "src_dx", "", map[string]any{"kind": "streak", "chance_period": "24h"})
	e.push(p)
	var title, summary string
	e.update(func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT json_extract(content, '$.title'), json_extract(content, '$.summary') FROM delivery_delivery`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			if err := r.Scan(&title, &summary); err != nil {
				return err
			}
		}
		return r.Err()
	})
	if want := "来源「src_dx」这次没读到"; title != want || summary != "这类内容每 24h 才有一次机会，所以失败一次就提醒。去来源页看失败原因；常见是本机工具没在跑或网络不通。" {
		t.Fatalf("title %q summary %q", title, summary)
	}
	e.event(domain.EvSourceFailed, "src_dx", "", map[string]any{"kind": "streak", "chance_period": "24h"}) // 同一串的下一次失败
	e.push(p)
	if n, ids := e.deliveries(); n != 1 || ids[0] != fmt.Sprintf("push:source:src_dx:%d:fail", last.Unix()) {
		t.Fatalf("one push per streak: %v", ids)
	}
}

// 新建议攒多了：默认关；打开后攒够 Min 条推一次，冷却期内不再推。
func TestPushNewProposalsThresholdAndCooldown(t *testing.T) {
	e := setup(t)
	p := NewPusher(e.att())
	e.push(p)
	created := func() {
		e.event(domain.EvItemCreated, "itm_x", "", map[string]any{"item": map[string]any{"id": "itm_x", "phase": "proposed"}})
	}
	created()
	created()
	e.push(p)
	if n, _ := e.deliveries(); n != 0 {
		t.Fatal("new proposals are off by default")
	}
	e.setPush(policy.PushNewProposals, policy.PushRule{On: true, Min: 2})
	e.update(func(tx *store.Tx) error { // 合并关闭：建立时就看攒够没有（开启时见 TestPushNewProposalsCountsAfterMerge）
		_, err := policy.Update(tx, owner.Actor, "merge", func(p *policy.Policy) error { p.MergeJudge = policy.MergeOff; return nil })
		return err
	})
	created()
	created()
	created()
	created()
	e.push(p)
	if n, ids := e.deliveries(); n != 1 {
		t.Fatalf("want one push within the cooldown, got %v", ids)
	}
}

// 跨来源合并开启时：建立时只计数，归类完（留着）才看攒够没有；折叠掉的扣回，所以按合并之后的条数推。
func TestPushNewProposalsCountsAfterMerge(t *testing.T) {
	e := setup(t)
	p := NewPusher(e.att())
	e.push(p)
	e.setPush(policy.PushNewProposals, policy.PushRule{On: true, Min: 2})
	item := func(id, phase, state string) map[string]any {
		return map[string]any{"id": id, "phase": phase, "proposal": map[string]any{"sweep": map[string]any{"state": state}}}
	}
	e.event(domain.EvItemCreated, "itm_a", "", map[string]any{"item": map[string]any{"id": "itm_a", "phase": "proposed"}})
	e.event(domain.EvItemCreated, "itm_b", "", map[string]any{"item": map[string]any{"id": "itm_b", "phase": "proposed"}})
	e.event(domain.EvItemUpdated, "itm_b", "", map[string]any{"op": "update", "item": item("itm_b", "proposed", "pending")})
	e.push(p)
	if n, ids := e.deliveries(); n != 0 {
		t.Fatalf("two created but not yet merged must not push: %v", ids)
	}
	e.event(domain.EvItemPhaseChanged, "itm_b", "", map[string]any{"op": "merge.fold", "item": item("itm_b", "dropped", "done")})
	e.event(domain.EvItemUpdated, "itm_a", "", map[string]any{"op": "update", "item": item("itm_a", "proposed", "done")})
	e.push(p)
	if n, ids := e.deliveries(); n != 0 {
		t.Fatalf("one proposal left after the merge is below the threshold, got %v", ids)
	}
	e.event(domain.EvItemCreated, "itm_c", "", map[string]any{"item": map[string]any{"id": "itm_c", "phase": "proposed"}})
	e.event(domain.EvItemUpdated, "itm_c", "", map[string]any{"op": "update", "item": item("itm_c", "proposed", "done")})
	e.push(p)
	if n, ids := e.deliveries(); n != 1 {
		t.Fatalf("two kept after merging reach the threshold, got %v", ids)
	}
}

// 简报没出来：简报这类开了注入主会话（默认）且没单独选去处时，这条也说在主会话里；关掉注入就跟「需要你决定」走。
func TestTemplateFailedFollowsTaskPush(t *testing.T) {
	e := setup(t)
	for _, err := range []error{items.Register(e.x, items.NewKinds()), executors.Register(e.x, executors.Options{Owner: "owner"})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	do := func(cmd string, in map[string]any) json.RawMessage {
		b, _ := json.Marshal(in)
		r, err := e.x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner"})
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return r.Value
	}
	do("executor.register", map[string]any{"harness": "hx", "main_session": map[string]any{"wake": true},
		"headless": map[string]any{"command": []string{"true"}, "session": map[string]any{"extract": "$.session_id"}, "access": map[string]any{"edit": []string{}}}})
	e.update(func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO instances_instance (id, pack, project, params, enabled) VALUES ('ins_b', 'brief', '', '{}', 1)`); err != nil {
			return err
		}
		_, err := policy.Update(tx, owner.Actor, "main", func(p *policy.Policy) error {
			main := "legacy:hx"
			p.MainAgentID, p.Disturb.Routes["decision"] = &main, "channel:chan"
			return nil
		})
		return err
	})
	brief := func() string {
		var out struct{ ID string }
		_ = json.Unmarshal(do("item.create", map[string]any{"kind": "brief", "title": "简报", "phase": "open", "assignee": "owner",
			"origin": map[string]any{"source": domain.OriginInstancePrefix + "ins_b"}}), &out)
		return out.ID
	}
	p := NewPusher(e.att())
	e.push(p)
	a := brief()
	e.event(domain.EvRunFinished, a, "", map[string]any{"category": "task", "state": "failed", "run_id": "run_1", "error": "exit status 1"})
	e.push(p)
	route := func(item string) (r string) {
		e.update(func(tx *store.Tx) error {
			rows, err := tx.Query(`SELECT route FROM delivery_delivery WHERE item = ?`, item)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if err := rows.Scan(&r); err != nil {
					return err
				}
			}
			return rows.Err()
		})
		return r
	}
	if r := route(a); r != "wake" {
		t.Fatalf("brief failure with push on: route %q, want the main session", r)
	}
	e.update(func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "off", func(p *policy.Policy) error {
			s := policy.DefaultProactive()
			s.TaskPush = map[string]bool{"brief": false}
			p.Disturb.Proactive = &s
			return nil
		})
		return err
	})
	b := brief()
	e.event(domain.EvRunFinished, b, "", map[string]any{"category": "task", "state": "failed", "run_id": "run_2", "error": "exit status 1"})
	e.push(p)
	if r := route(b); r != "channel:chan" {
		t.Fatalf("brief failure with push off: route %q, want the decision route", r)
	}
}

// 暂停所有提醒（mutes 范围 all）时：来源连续失败、Agent 运行失败这两类健康推送照常送到渠道；
// 同一时段里的其它类（新建议攒多了，提醒类）只留 Web 未读、记 muted。
func TestPauseAllLetsHealthPushesThrough(t *testing.T) {
	e := setup(t)
	for _, err := range []error{items.Register(e.x, items.NewKinds()), executors.Register(e.x, executors.Options{Owner: "owner"})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	e.update(func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "pause", func(p *policy.Policy) error {
			p.Disturb.Routes["decision"], p.Disturb.Routes["reminder"] = "channel:chan", "channel:chan"
			p.Disturb.Mutes = []policy.Mute{{Scope: policy.MuteAll}}
			p.MergeJudge = policy.MergeOff
			return nil
		})
		return err
	})
	e.setPush(policy.PushNewProposals, policy.PushRule{On: true, Min: 1})
	p := NewPusher(e.att())
	e.push(p)
	b, _ := json.Marshal(map[string]any{"kind": "task", "title": "整理周报", "phase": "open", "assignee": "owner"})
	r, err := e.x.Do(bg, command.Request{Identity: owner, Command: "item.create", Input: b, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	var task struct{ ID string }
	_ = json.Unmarshal(r.Value, &task)
	e.source("src_x", true, runner.Stats{ConsecutiveFailures: 3, LastSuccessAt: t0.Add(-time.Hour)})
	e.event(domain.EvSourceFailed, "src_x", "", map[string]any{"kind": "streak"})
	e.event(domain.EvRunFinished, task.ID, "", map[string]any{"category": "task", "state": "failed", "run_id": "run_1", "error": "exit status 1"})
	e.event(domain.EvItemCreated, "item_p", "", map[string]any{"item": map[string]any{"phase": "proposed", "proposal": map[string]any{}}})
	e.push(p)
	got := map[string]string{}
	e.update(func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT id, route, suppressed FROM delivery_delivery`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, route, sup string
			if err := rows.Scan(&id, &route, &sup); err != nil {
				return err
			}
			got[id[:strings.Index(id[5:], ":")+5]] = route + "|" + sup
		}
		return rows.Err()
	})
	want := map[string]string{"push:source": "channel:chan|", "push:run": "channel:chan|", "push:proposals": "web|muted"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paused: got %v, want %v", got, want)
	}
}
