//go:build e2e

package workbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// tierScript 是 tier-src.sh：按 §9.2 执行契约每次输出两条标准事件（acc.match、acc.other），id 每次递增。
const tierScript = `#!/bin/sh
Q="$1"
cat > /dev/null
n=$(cat "$Q/n" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$Q/n"
# 前三次运行才产出事件：之后的核对面对的是一份不再增长的积压（来源仍按周期运行，只是没有新事件）。
if [ "$n" -gt 3 ]; then echo '{"cursor":"'$n'"}'; exit 0; fi
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo '{"id":"m-'$n'","type":"acc.match","subject":"match '$n'","occurred_at":"'$now'","sender_verified":true}'
echo '{"id":"o-'$n'","type":"acc.other","subject":"other '$n'","occurred_at":"'$now'","sender_verified":true}'
echo '{"cursor":"'$n'"}'
`

// newTier1 建立只有内核的实例（ACT-TIER-01）：不配判断端点与生成端点，不提交无头运行声明，不启动任何替身。
func newTier1(t *testing.T) *kit {
	t.Helper()
	e := baseline.New(t)
	k := &kit{Env: e, t: t}
	e.Init()
	// 内置 follow-up 包复制到数据目录：follow-up-interval 改 1m，stale-check 改 stale_after 3m、schedule "*/2 * * * *"。
	k.copyFollowUpPack(func(s string) string {
		s = strings.Replace(s, "schedule: 72h", "schedule: 1m", 1)
		return strings.Replace(s, "schedule: 24h\n    stale_after: 14d", "schedule: \"*/2 * * * *\"\n    stale_after: 3m", 1)
	})
	k.writePack("acc-tier", `name: acc-tier
version: 1.0.0
description: 验收用来源规则（数据目录包，不属于发行）
source_rules:
  - id: acc-tier-match
    match: {source: src_tier, event_type: [acc.match]}
    kind: task
    project_by: none
    then: proposed
`)
	e.Serve()
	e.IssueCredentials() // T1_OBS、T1_WRK（及其余凭据，第一档不需要）
	e.CreateProjects()
	return k
}

func (k *kit) registerTierSource() {
	k.t.Helper()
	q := filepath.Join(k.Root, "src-tier")
	k.must(os.MkdirAll(q, 0o755))
	script := filepath.Join(q, "tier-src.sh")
	k.must(os.WriteFile(script, []byte(tierScript), 0o755))
	k.webOK("POST", "/sources", `{"id":"src_tier","type":"script","source_type":"acc-tier","command":"`+script+` `+q+`","schedule":"1s","timeout":30}`, 201)
}

// TestFlowRunTiers 走完 flow-run-tiers：只有内核的第一档自成可用产品——不配模型端点、不接宿主时工作台、提醒和积压照常。
func TestFlowRunTiers(t *testing.T) {
	k := newTier1(t)
	TODAY := day(0)

	k.step("ACT-TIER-01 建立只有内核的实例", func(t *testing.T) {
		k.registerTierSource()
		k.waitEvents("src_tier", 2) // 有事件在等判断，健康视图才会报端点没配置
		_, sigs := k.health(k.OBS)
		d, _ := sigs["model_unavailable"].Detail.(map[string]any)
		eq(t, "model_unavailable.detail.judge", d["judge"], "not_configured")
		eq(t, "model_unavailable.detail.generate", d["generate"], "not_configured")
		if p := k.webOK("GET", "/packs", "", 200); !strings.Contains(p, `"name":"acc-tier"`) {
			t.Error("acc-tier 包没有加载")
		}
		if k.Cls != nil || k.ModelStub != nil {
			t.Error("第一档实例不应有任何模型端点替身")
		}
	})

	k.step("ACT-TIER-02 第一档工作台可用", func(t *testing.T) {
		id := k.webItem(map[string]any{"kind": "task", "title": "T1-工作台", "project": k.Projects["acc-home"], "planned_for": TODAY})
		eq(t, "planned_for", k.planned(id), TODAY)
		byOwner(t, "新建", k.lastEvent(id, "item.created"))
		k.webOK("POST", "/items/"+id+"/progress", `{"text":"第一档写进展"}`, 201)
		byOwner(t, "进展", k.lastEvent(id, "item.progress_added"))
		if !has(k.today().ids("planned"), id) {
			t.Errorf("今天清单缺该事项: %v", k.today().ids("planned"))
		}
		k.webDone(id)
		byOwner(t, "完成", k.lastEvent(id, "item.phase_changed"))
		eq(t, "阶段", k.phase(id), "done")
		if has(k.today().ids("planned"), id) {
			t.Error("完成后仍在 planned 组")
		}
	})

	var fu, dl, stt string
	k.step("ACT-TIER-03 第一档跟进、截止与久未推进提醒照常", func(t *testing.T) {
		// 第一档没有宿主领取：默认的"提醒"路由是注入，无人领取 30 分钟后才转 Web 未读（§6.9）。
		// 只有 Web 的本人把提醒与决策路由改成 Web（打扰控制的设置，不是规则模式），截止提醒保持默认开启。
		k.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true}}`, 200)
		fu = idOfOut(t, k.apa(k.WRK, 0, "item", "add", "--kind", "task", "--title", "T-FU", "--assignee", "work-host.cli", "--project", k.Projects["acc-work"]).Stdout)
		// worker 只能以 proposed 新建：先由本人在建议流接受，再由 worker 转 waiting（离开 proposed 按代答判定，worker 不能自己确认）。
		eq(t, "worker 新建的阶段", k.phase(fu), "proposed")
		k.webOK("POST", "/items/"+fu+"/accept", `{}`, 200)
		k.apa(k.WRK, 0, "item", "transition", fu, "--to", "waiting", "--waiting-on", "external:供应商回复", "--reason", "等供应商")
		deadline := time.Now().Add(time.Minute).Format(time.RFC3339)
		dl = k.webItem(map[string]any{"kind": "task", "title": "T-DL", "deadline": deadline})
		stt = k.webItem(map[string]any{"kind": "task", "title": "T-ST"})
		// 等 5 分钟（测试时钟）：不碰任何事项，也不切换任何规则模式。
		// 跟进梯度每轮的提醒会被下一轮（以及放弃前的问去留）以同一投递的新版本替换，所以每步都看一眼未读。
		sawRound := false
		for done := 0; done < 300; done += 30 {
			k.advance(30, 30)
			un := k.unreadRaw()
			if strings.Contains(un, fu) && strings.Contains(un, `"class":"reminder"`) {
				for _, d := range k.deliveriesOf(fu) {
					if d.Class == "reminder" && strings.Contains(d.Body, "已催") && strings.Contains(d.Body, "再等") && strings.Contains(d.Body, "放弃") {
						sawRound = true
					}
				}
			}
		}
		if !sawRound {
			t.Errorf("T-FU 没有出现带\"已催\"\"再等\"\"放弃\"的跟进提醒: %s", k.unreadRaw())
		}
		un := k.unreadRaw()
		if !strings.Contains(un, "放弃") || !strings.Contains(un, fu) {
			t.Errorf("T-FU 轮次用完后没有问去留（放弃）: %s", un)
		}
		if !strings.Contains(un, dl) {
			t.Errorf("没有 T-DL 的到期提醒: %s", un)
		}
		if !has(k.today().ids("stale"), stt) {
			t.Errorf("T-ST 没有进入久未推进组: %v", k.today().ids("stale"))
		}
		if k.remindersMentioning("T-ST") != 1 {
			t.Errorf("T-ST 没有恰好一条合并的久未推进提醒: %s", un)
		}
	})

	var matches, others []SrcEvent
	k.step("ACT-TIER-04 规则能判定的进建议流，规则判不了的留在积压", func(t *testing.T) {
		var evs []SrcEvent
		if !k.WaitFor(60*time.Second, func() bool {
			evs = k.waitEvents("src_tier", 4) // 至少两轮、每轮两条
			return len(evs) >= 4
		}) {
			t.Fatal("tier-src 没有运行至少两次")
		}
		// 来源脚本只在前三次运行时产出事件（见 tierScript），等已拉到的事件全部处置完，再按同一份快照核对。
		time.Sleep(3 * time.Second)
		evs = k.waitEvents("src_tier", 4)
		matches, others = nil, nil
		for _, e := range evs {
			switch e.Type {
			case "acc.match":
				matches = append(matches, e)
			case "acc.other":
				others = append(others, e)
			}
		}
		for _, e := range matches {
			eq(t, e.Subject+" 处置", e.Disposition, "proposed")
			eq(t, e.Subject+" 阶段", k.phase(e.Item), "proposed")
		}
		for _, e := range others {
			eq(t, e.Subject+" 处置", e.Disposition, "unjudged")
			eq(t, e.Subject+" 原因", e.UnjudgedReason, "model_unavailable")
		}
		if len(matches) < 2 || len(others) < 2 {
			t.Fatalf("事件数不足: match %d、other %d", len(matches), len(others))
		}
		backlog := k.srcEvents("src_tier", "unjudged,expired")
		eq(t, "来源页积压条数", len(backlog), len(others))
		healthy, sigs := k.health(k.OBS)
		s := sigs["unjudged_backlog"]
		eq(t, "unjudged_backlog.ok", s.OK, false)
		eq(t, "unjudged_backlog.severity", s.Severity, "warning")
		eq(t, "unjudged_backlog.count", s.Count, len(backlog))
		// healthy 只看 error 级信号：unjudged_backlog 是 warning，不影响它。第一档没配端点而有事件在等时，
		// model_unavailable（error，v0.1 起的信号）会让 healthy 为 false——那是它自己的原因，不是积压。
		errors := 0
		for id, sg := range sigs {
			if !sg.OK && sg.Severity == "error" {
				errors++
				eq(t, "唯一的 error 级信号", id, "model_unavailable")
			}
		}
		eq(t, "healthy 与 error 级信号一致", healthy, errors == 0)
	})

	k.step("ACT-TIER-05 本人可手动处理积压", func(t *testing.T) {
		toPropose, toIgnore := others[0], others[1]
		k.webOK("POST", "/source-events/"+toPropose.ID+"/propose", "", 200)
		k.webOK("POST", "/source-events/"+toIgnore.ID+"/ignore", "", 200)
		var proposed, ignored SrcEvent
		for _, e := range k.srcEvents("src_tier", "") {
			switch e.ID {
			case toPropose.ID:
				proposed = e
			case toIgnore.ID:
				ignored = e
			}
		}
		eq(t, "转为建议后的处置", proposed.Disposition, "proposed")
		eq(t, "忽略后的处置", ignored.Disposition, "ignored")
		if proposed.Item == "" {
			t.Fatalf("转为建议没有生成事项: %+v", proposed)
		}
		eq(t, "转出的建议阶段", k.phase(proposed.Item), "proposed")
		k.webOK("POST", "/items/"+proposed.Item+"/accept", `{}`, 200)
		eq(t, "接受后的阶段", k.phase(proposed.Item), "open")
		byOwner(t, "转为建议", k.lastEvent(proposed.ID, "inbound.disposed"))
		byOwner(t, "忽略", k.lastEvent(ignored.ID, "inbound.disposed"))
		byOwner(t, "接受", k.lastEvent(proposed.Item, "item.phase_changed"))
	})

	k.step("ACT-TIER-06 清理第一档实例", func(t *testing.T) {
		pid := k.ServePID()
		k.StopKernel()
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("T1 进程 %d 仍然存在", pid)
		}
	})
}

func idOfOut(t *testing.T, s string) string {
	t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	jsonOf(t, s, &m)
	if m.ID == "" {
		t.Fatalf("没有返回 id: %s", s)
	}
	return m.ID
}

type deliveryView struct {
	Class string
	Body  string // 投递整条 JSON，用来查提醒内容
}

// deliveriesOf 取 Web 未读里属于某事项的投递。
func (k *kit) deliveriesOf(item string) []deliveryView {
	k.t.Helper()
	var out struct {
		Deliveries []json.RawMessage `json:"deliveries"`
	}
	jsonOf(k.t, k.unreadRaw(), &out)
	var res []deliveryView
	for _, raw := range out.Deliveries {
		var d struct {
			Item  string `json:"item"`
			Class string `json:"class"`
		}
		jsonOf(k.t, string(raw), &d)
		if d.Item == item {
			res = append(res, deliveryView{d.Class, string(raw)})
		}
	}
	return res
}
