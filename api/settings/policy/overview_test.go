package policy

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	krules "github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type packList []*packs.Pack

func (l packList) List() []*packs.Pack { return l }

// 总览的行表就是矩阵的行表加每个实例两行；计数按选去处同一个 rowOf 归行，两层算一条提醒、两条推送，
// 只留今天页的与被暂停压下的不算推送，合并摘要不计；没有产生方的行标出来；来源提醒线取 runner 的常量。
func TestOverviewRowsCountsAndProducers(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, command.Module, kpolicy.Module, items.Module, instances.Module, delivery.Module, registry.Module, krules.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	q := delivery.New(delivery.Options{Now: func() time.Time { return now }})
	past := now.Add(-time.Hour)
	err = st.Update(bg, func(tx *store.Tx) error {
		for _, d := range []domain.Delivery{
			{ID: "decision:itm_a:1", Item: "itm_a", Class: domain.DeliveryDecision, Route: "channel:ch-1", Version: 1},
			{ID: "decision:itm_a:1@agent", Item: "itm_a", Class: domain.DeliveryDecision, Route: "wake:agt_1", Version: 1},
			{ID: "remind:due:itm_b:1", Item: "itm_b", Class: domain.DeliveryReminder, Route: delivery.RouteWeb, Version: 1},
			{ID: "push:source:src_mail:0:fail", Item: "src_mail", Class: domain.DeliveryHealth, Route: delivery.RouteWeb, Suppressed: delivery.SuppressedMuted, Version: 1},
			{ID: "digest:abc", Item: "attention:digest", Class: domain.DeliveryReminder, Route: "channel:ch-1", Version: 1},
		} {
			d.Content = domain.DeliveryContent{Title: d.ID}
			if _, _, err := q.Create(tx, d); err != nil {
				return err
			}
		}
		for _, it := range []domain.Item{
			{ID: "itm_q", Kind: "task", Title: "q", Phase: domain.PhaseNeedsInput, Ask: &domain.Ask{AskInput: domain.AskInput{Question: "?", FreeText: true}}},
			{ID: "itm_w", Kind: "task", Title: "w", Phase: domain.PhaseWaiting, WaitingOn: &domain.WaitingOn{Kind: "external", Ref: "x"}},
			{ID: "itm_d", Kind: "task", Title: "d", Phase: domain.PhaseOpen, Deadline: &past},
		} {
			if _, _, err := items.Create(tx, it, "", now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO instances_instance (id, pack, project, params, enabled) VALUES ('ins_b', 'brief', '', '{}', 1), ('ins_r', 'review', '', '{}', 1)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO rules_state (key, pack, grp, id, mode, hash) VALUES ('review/clock_rules/scheduled-review', 'review', 'clock_rules', 'scheduled-review', 'off', '')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path,
			dedupe_domain, config, cursor, enabled, stats, default_project, outbound_local, outbound_external, name)
			VALUES ('src_mail', 'script', 'mail', 'x', '10m', 30, '', '', '', '', 1, '{}', '', 'full', '', '邮件'),
			       ('src_daily', 'script', 'chat', 'x', '24h', 30, '', '', '', '', 1, '{}', '', 'full', '', '日报')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var o Overview
	if err := st.View(bg, func(tx *store.Tx) (err error) {
		o, err = overview(tx, packList{}, 14*24*time.Hour, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := map[string]overviewRow{}
	var keys []string
	for _, r := range o.Rows {
		got[r.Key] = r
		keys = append(keys, r.Key)
	}
	want := append(append([]string{}, kpolicy.FixedRows...), "instance:ins_b", "instance_failed:ins_b", "instance:ins_r", "instance_failed:ins_r")
	if len(keys) != len(want) {
		t.Fatalf("rows %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("row %d = %s, want %s", i, keys[i], want[i])
		}
	}
	for k, c := range map[string]attention2{
		"decision":          {1, 2, true},
		"overdue":           {1, 0, true},
		"source_failing":    {1, 0, true},
		"rule_reminder":     {0, 0, false},
		"source_alert":      {0, 0, false},
		"task_result:brief": {0, 0, false},
		"task_failed:other": {0, 0, false},
		"instance:ins_b":    {0, 0, true},
	} {
		if r := got[k]; r.Events != c.events || r.Pushed != c.pushed || r.Produced != c.produced {
			t.Errorf("%s = %+v, want %+v", k, r, c)
		}
	}
	if got["instance:ins_r"].Blocked != "rule_off" || got["instance:ins_b"].Blocked != "" {
		t.Errorf("blocked: review %q brief %q", got["instance:ins_r"].Blocked, got["instance:ins_b"].Blocked)
	}
	if n := o.Now; n.DecisionsPending != 1 || n.Waiting != 1 || n.Deadlines != 1 || n.Overdue != 1 || n.StaleNow != 0 || n.StaleFrom == nil || !n.StaleFrom.Equal(now.Add(14*24*time.Hour)) {
		t.Errorf("now = %+v", n)
	}
	a := o.SourceAlert
	if a.StreakFailures != 3 || a.LowFrequencyOverSec != 3600 || len(a.Sources) != 2 || a.Sources[0].ID != "src_daily" || a.Sources[0].AlertAfter != 1 || a.Sources[1].AlertAfter != 3 {
		t.Errorf("source alert = %+v", a)
	}
}

type attention2 struct {
	events, pushed int
	produced       bool
}

// 总览只给 Web 本人，只读。
func TestOverviewIsWebOnly(t *testing.T) {
	e := setup(t, OverviewRoutes(nil, packList{}, func() time.Duration { return time.Hour }))
	if rec := e.do("GET", "/v1/settings/notify/overview", "", e.bearer(auth.Delegate)); rec.Code != 403 {
		t.Fatalf("delegate: %d %s", rec.Code, rec.Body)
	}
}
