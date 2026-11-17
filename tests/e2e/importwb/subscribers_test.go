//go:build e2e

package importwb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// 规则、触达、跟进三个订阅方的"现场对照 + 导入不触发"：每一项都先让同类的现场事件使订阅方留下可见的变化（对照，证明订阅方在工作、断言能失败），
// 再断言带导入标记的同类事件没有这个变化。把对应订阅方的 eventlog.ReadLive 改成 eventlog.Read，下面三个步骤各自必须失败。

func (f *fx) cursorSeq(table string) int64 {
	f.t.Helper()
	var n int64
	_ = f.ledger().QueryRow("SELECT seq FROM " + table + " WHERE id = 1").Scan(&n) // 表名是本文件写死的
	return n
}

// caughtUp 等给定订阅方的事件游标追到最后一条现场事件：之后的"没有变化"才是它处理过之后的结论。
// 游标只越过读到的事件，账本末尾的导入事件读不到（这正是"现场"读法），所以头取最后一条非导入事件。
func (f *fx) caughtUp(cursors ...string) {
	f.t.Helper()
	var head int64
	if err := f.ledger().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM eventlog_event WHERE import_batch = ''`).Scan(&head); err != nil {
		f.t.Fatal(err)
	}
	f.until(fmt.Sprintf("%v 追到 %d", cursors, head), func() bool {
		for _, c := range cursors {
			if f.cursorSeq(c) < head {
				return false
			}
		}
		return true
	})
}

// inject 在内核停下时向账本直接写事件或改投影（只在测试目录里，写法与内核单测一致：eventlog.Append），再启动内核。
func (f *fx) inject(fn func(tx *store.Tx, now time.Time) error) {
	f.t.Helper()
	f.StopKernel()
	reg, err := ledgerfixture.RegistryAt(1 << 30)
	if err != nil {
		f.t.Fatal(err)
	}
	st, err := store.Open(f.Home, reg)
	if err != nil {
		f.t.Fatal(err)
	}
	now := f.now()
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return fn(tx, now) }); err != nil {
		st.Close()
		f.t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.StartKernelAgain()
	f.relogin()
}

var owner = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}

func appendEv(tx *store.Tx, typ, object string, now time.Time, batch string) error {
	ev := domain.Event{Type: typ, Object: object, Actor: owner, Payload: json.RawMessage(`{}`), At: now}
	if batch != "" {
		ev.Imported, ev.RecordedAt = batch, now
	}
	_, err := eventlog.Append(tx, ev)
	return err
}

func (f *fx) writeRulePack() {
	f.t.Helper()
	dir := filepath.Join(f.Home, "packs", "acc-imp-rules")
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	writeFile(f.t, filepath.Join(dir, "prompts", "acc-imp-watch.md"), "---\nagent: task\naccess: read\ntimeout: 1m\n---\nwatch {item_id}\n")
	writeFile(f.t, filepath.Join(dir, "pack.yaml"), `name: acc-imp-rules
version: 1.0.0
description: 导入验收夹具：带标签的事项转 done 时观察
event_rules:
  - id: watch-done
    mode: observe
    when:
      event: item.phase_changed
      to_phase: done
      tag_any: [acc-imp-watch]
    do:
      - action: call_agent
        prompt: acc-imp-watch
`)
	f.until("能力包 acc-imp-rules 被加载", func() bool {
		return strings.Contains(f.APIRaw("GET", "/v1/packs", "", f.OBS).Body, `"acc-imp-rules"`)
	})
}

// TestFlowImportNoAutomationSubscribers 是 flow-import-no-automation 里规则、触达、跟进三个订阅方的敏感性断言
// （OMAC-NOTES §3 I5、GT-390 清单）：同类事件，现场写入有可见变化，带导入标记写入没有。
func TestFlowImportNoAutomationSubscribers(t *testing.T) {
	f := setup(t)
	at := func(back time.Duration) string { return rfc(f.now().Add(-back).In(cst).Truncate(time.Second)) }

	f.step("规则：现场的 item.phase_changed→done 被观察，同类导入事件不被观察", func(t *testing.T) {
		f.writeRulePack()
		observed := func() int { return f.eventCount(`type = 'rule.observed'`) }
		live := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "规则对照", "--phase", "open", "--tags", "acc-imp-watch", "--evidence", "对照").Stdout).ID
		f.apa(f.DEL, 0, "item", "transition", live, "--to", "done", "--reason", "对照", "--evidence", "对照")
		f.until("现场事件被规则观察到", func() bool { return observed() >= 1 })
		f.caughtUp("rules_cursor")
		n, fires := observed(), f.tableCounts()["rules_fire"]
		line := fmt.Sprintf(`{"type":"item","external_id":"R-1","item":{"kind":"task","title":"导入的带标签事项","tags":["acc-imp-watch"]},"history":[{"at":%q,"event":"created","phase":"open"},{"at":%q,"event":"transition","to":"done"}]}`+"\n", at(72*time.Hour), at(48*time.Hour))
		f.mustImport("acc-imp-rules", "", line)
		f.caughtUp("rules_cursor")
		if got := observed(); got != n {
			t.Errorf("导入的 phase_changed→done 被规则观察了: rule.observed %d -> %d", n, got)
		}
		if got := f.tableCounts()["rules_fire"]; got != fires {
			t.Errorf("导入触发了规则: rules_fire %d -> %d", fires, got)
		}
	})

	f.step("触达：现场的 item.updated 使投递按现状重算，同类导入事件不重算", func(t *testing.T) {
		ask := func(mark string) string {
			id := decode[struct {
				ID string `json:"id"`
			}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "待决策 "+mark, "--phase", "open", "--assignee", "work-host.cli", "--project", f.Projects["acc-work"], "--evidence", "夹具").Stdout).ID
			f.apa(f.WRK, 0, "ask", id, "--question", "选哪个 "+mark, "--option", "A=甲", "--option", "B=乙")
			return id
		}
		a, b := ask("导入"), ask("现场")
		row := func(id string) string {
			return strings.Join(f.queryStrings(`SELECT state || '|' || version FROM delivery_delivery WHERE item = ? ORDER BY created_at`, id), ";")
		}
		f.until("两条待决策各有一条投递", func() bool { return row(a) != "" && row(b) != "" })
		beforeA, beforeB := row(a), row(b)
		// 事项现状变了（不再是待决策）而没有普通事件：此后谁再来一个 item.updated，谁的投递就会被重算掉。
		f.inject(func(tx *store.Tx, now time.Time) error {
			for _, id := range []string{a, b} {
				if _, err := tx.Exec(`UPDATE items SET phase = 'done', data = json_set(data, '$.phase', 'done') WHERE id = ?`, id); err != nil {
					return err
				}
			}
			if err := appendEv(tx, domain.EvItemUpdated, a, now, "imp_acc"); err != nil { // 导入的
				return err
			}
			return appendEv(tx, domain.EvItemUpdated, b, now, "") // 现场的
		})
		f.until("现场对照：现场的 item.updated 使它的投递被重算", func() bool { return row(b) != beforeB })
		if got := row(a); got != beforeA {
			t.Errorf("导入的 item.updated 使投递被重算了: %q -> %q", beforeA, got)
		}
	})

	f.step("跟进：现场的进展清零轮次，同类导入事件不清零", func(t *testing.T) {
		wait := func(id string) string {
			return fmt.Sprintf(`{"type":"item","external_id":%q,"item":{"kind":"task","title":"导入的等待事项 %s","waiting_on":{"kind":"human","ref":"某人"}},"history":[{"at":%q,"event":"created","phase":"open"},{"at":%q,"event":"transition","to":"waiting"}]}`+"\n",
				id, id, at(72*time.Hour), at(48*time.Hour))
		}
		out := f.mustImport("acc-fu", "", wait("W-A")+wait("W-B"))
		a, b := out.Results[0].Object, out.Results[1].Object
		rounds := func(id string) string {
			return strings.Join(f.queryStrings(`SELECT CAST(rounds AS TEXT) FROM followup_state WHERE item = ?`, id), "")
		}
		f.advance(int64((72*time.Hour + time.Minute).Seconds())) // 第一轮到期：两件各记一轮
		f.until("两件等待事项各已跟进一轮", func() bool { return rounds(a) == "1" && rounds(b) == "1" })
		f.inject(func(tx *store.Tx, now time.Time) error {
			if err := appendEv(tx, domain.EvItemProgressAdded, a, now, "imp_acc"); err != nil { // 导入的进展
				return err
			}
			return appendEv(tx, domain.EvItemProgressAdded, b, now, "") // 现场的进展
		})
		f.until("现场对照：现场的进展使轮次清零", func() bool { return rounds(b) == "0" })
		if got := rounds(a); got != "1" {
			t.Errorf("导入的进展清零了跟进轮次: rounds = %q，期望 1", got)
		}
	})
}
