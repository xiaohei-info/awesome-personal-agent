package health

import (
	"fmt"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
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
	p := NewPusher(e.q)
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

// 新建议攒多了：默认关；打开后攒够 Min 条推一次，冷却期内不再推。
func TestPushNewProposalsThresholdAndCooldown(t *testing.T) {
	e := setup(t)
	p := NewPusher(e.q)
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
	created()
	created()
	created()
	created()
	e.push(p)
	if n, ids := e.deliveries(); n != 1 {
		t.Fatalf("want one push within the cooldown, got %v", ids)
	}
}
