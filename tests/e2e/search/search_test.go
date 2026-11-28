//go:build e2e

// Package search 是统一检索与分层读取（主文档 §7.1、§17.2、决定 99）的全流程验收：真实 apa 二进制 + 真实内核进程。
// 空白 Agent（只持 observer 钥匙、没有任何上下文）三次调用以内找到一件事的来龙去脉；跨项目整句检索；英文词形还原；
// 已放弃的默认不出；零命中计数在健康视图可见；每次读取不超过 4000 字并给出下一步。
package search

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const budget = 4000 // 一次读取的字数上限（kernel/context.InjectBudget）

type fx struct {
	*baseline.Env
	t *testing.T
}

func step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func (f *fx) apa(token string, args ...string) string {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != 0 {
		f.t.Fatalf("apa %s 退出码 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r.Stdout
}

// read 是空白 Agent 的一次读取：输出不超过上限，返回解出的 JSON 与字数。
func (f *fx) read(t *testing.T, args ...string) (map[string]any, int) {
	t.Helper()
	out := strings.TrimSpace(f.apa(f.OBS, args...))
	n := utf8.RuneCountInString(out)
	if n > budget {
		t.Fatalf("apa %s 输出 %d 字，超过上限 %d", strings.Join(args, " "), n, budget)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("apa %s 输出不是 JSON: %v\n%s", strings.Join(args, " "), err, out)
	}
	return m, n
}

func (f *fx) item(project, title string, comments ...string) string {
	f.t.Helper()
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.DEL, "item", "add", "--kind", "task", "--title", title, "--phase", "open",
		"--project", f.Projects[project])), &r); err != nil || r.ID == "" {
		f.t.Fatalf("建事项 %s: %v", title, err)
	}
	for _, c := range comments {
		f.apa(f.DEL, "item", "comment", r.ID, "--text", c)
	}
	return r.ID
}

func (f *fx) to(id, phase string) {
	f.t.Helper()
	f.apa(f.DEL, "item", "transition", id, "--to", phase, "--reason", "e2e")
}

func hitIDs(m map[string]any) []string {
	var ids []string
	hits, _ := m["hits"].([]any)
	for _, h := range hits {
		ids = append(ids, h.(map[string]any)["id"].(string))
	}
	return ids
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, x.(string))
	}
	return out
}

func TestFlowSearch(t *testing.T) {
	f := &fx{Env: baseline.Setup(t), t: t}
	target := f.item("acc-open", "星图按预算比例设上下限", "上限按预算的 30%，下限 10%", "复盘后改为按日均预算算比例")
	related := f.item("acc-open", "预算规则梳理", "旧规则是固定金额")
	f.to(related, "done")
	f.item("acc-open", "无关的事")
	inTitle := f.item("acc-home", "北辰加账号申请")
	inComment := f.item("acc-work", "账号整理", "北辰需要加账号")
	english := f.item("acc-work", "Merging the release branch")
	gone := f.item("acc-open", "星图旧方案")
	f.to(gone, "dropped")

	step(t, "ACT-SRCH-01 空白 Agent 三次调用找到来龙去脉与当前设置", func(t *testing.T) {
		found, n1 := f.read(t, "search", "星图按预算比例设上下限")
		ids := hitIDs(found)
		if len(ids) == 0 || ids[0] != target || !slices.Contains(ids, related) {
			t.Fatalf("检索结果: %v", ids)
		}
		if next := strs(found["next"]); len(next) == 0 || next[0] != "apa item show "+target {
			t.Fatalf("第一条下一步应是读第一名: %v", next)
		}
		shown, n2 := f.read(t, "item", "show", target)
		progress, _ := shown["progress"].([]any)
		if len(progress) != 2 || !strings.Contains(progress[1].(map[string]any)["text"].(string), "日均") {
			t.Fatalf("当前设置应在最近的进展里: %v", progress)
		}
		why, n3 := f.read(t, "item", "thread", target)
		if len(why["entries"].([]any)) == 0 {
			t.Fatalf("讨论流为空: %v", why)
		}
		if total := n1 + n2 + n3; total > 3*budget {
			t.Fatalf("三次读取合计 %d 字", total)
		}
	})

	step(t, "ACT-SRCH-02 跨项目整句检索，命中标题与命中进展的都在前两名", func(t *testing.T) {
		m, _ := f.read(t, "search", "北辰 加账号")
		ids := hitIDs(m)
		if len(ids) < 2 || !slices.Contains(ids[:2], inTitle) || !slices.Contains(ids[:2], inComment) {
			t.Fatalf("跨项目: %v", ids)
		}
		for _, h := range m["hits"].([]any)[:2] {
			hit := h.(map[string]any)
			if hit["coverage"] != "3/3" || len(hit["matched_in"].([]any)) == 0 || hit["project_name"] == "" {
				t.Fatalf("命中应写明位置、覆盖率与项目名: %v", hit)
			}
		}
	})

	step(t, "ACT-SRCH-03 英文词形还原：merged 命中 Merging", func(t *testing.T) {
		if ids := hitIDs(func() map[string]any { m, _ := f.read(t, "search", "merged"); return m }()); len(ids) == 0 || ids[0] != english {
			t.Fatalf("merged: %v", ids)
		}
	})

	step(t, "ACT-SRCH-04 已放弃的默认不出，写明另有几条，显式要时出现", func(t *testing.T) {
		m, _ := f.read(t, "search", "星图")
		if slices.Contains(hitIDs(m), gone) || m["withheld"].(map[string]any)["dropped"] != float64(1) {
			t.Fatalf("默认: %v", m)
		}
		if m, _ := f.read(t, "search", "星图", "--include-dropped"); !slices.Contains(hitIDs(m), gone) {
			t.Fatalf("--include-dropped: %v", hitIDs(m))
		}
	})

	step(t, "ACT-SRCH-05 零命中给出口并计入健康视图", func(t *testing.T) {
		m, _ := f.read(t, "search", "zzqqxxnothing")
		if m["total"] != float64(0) || len(strs(m["next"])) == 0 {
			t.Fatalf("零命中: %v", m)
		}
		var h struct {
			Search struct{ Queries, Zero int } `json:"search"`
		}
		if err := json.Unmarshal([]byte(f.webOK("GET", "/v1/health")), &h); err != nil || h.Search.Zero < 1 || h.Search.Queries < 5 {
			t.Fatalf("健康视图的检索统计: %+v %v", h, err)
		}
	})

	step(t, "ACT-SRCH-06 列表有界，按游标翻完", func(t *testing.T) {
		for i := range 32 {
			f.item("acc-empty", "批量事项 "+strconv.Itoa(i))
		}
		seen := map[string]bool{}
		args := []string{"item", "list", "--project", f.Projects["acc-empty"]}
		for page := 0; ; page++ {
			m, _ := f.read(t, args...)
			rows := m["items"].([]any)
			if len(rows) > 30 || m["total"] != float64(32) {
				t.Fatalf("一页 %d 行，total %v", len(rows), m["total"])
			}
			for _, r := range rows {
				seen[r.(map[string]any)["id"].(string)] = true
			}
			cur, _ := m["next_cursor"].(string)
			if cur == "" || page > 5 {
				break
			}
			args = []string{"item", "list", "--project", f.Projects["acc-empty"], "--cursor", cur}
		}
		if len(seen) != 32 {
			t.Fatalf("翻完应见到 32 条，见到 %d", len(seen))
		}
	})

	step(t, "ACT-SRCH-07 逐级往下：项目清单 → 项目卡片与规范 → 事项", func(t *testing.T) {
		l, _ := f.read(t, "project", "list")
		if len(strs(l["next"])) == 0 || l["total"] != float64(len(l["projects"].([]any))) {
			t.Fatalf("项目清单: %v", l)
		}
		var g struct {
			Material string `json:"material"`
		}
		json.Unmarshal([]byte(f.Apa(f.DEL, "按预算比例设上下限，改动先写进事项进展。\n", "material", "commit", "--from", "-", "--reason", "本人确认的约定",
			"--kind", "guideline", "--title", "acc-open 规范", "--project", f.Projects["acc-open"]).Stdout), &g)
		if g.Material == "" {
			t.Fatal("没有建出规范")
		}
		p, _ := f.read(t, "project", "show", f.Projects["acc-open"])
		card := p["card"].(map[string]any)
		if card["guideline"] != g.Material || card["counts"].(map[string]any)["open"] == nil {
			t.Fatalf("项目卡片: %v", card)
		}
		if next := strs(p["next"]); len(next) != 3 || next[2] != "apa material show "+g.Material {
			t.Fatalf("项目的下一步应指向规范: %v", next)
		}
	})
}

func (f *fx) webOK(method, path string) string {
	f.t.Helper()
	r := f.Web(method, path, "")
	if r.Code != 200 {
		f.t.Fatalf("Web %s %s: HTTP %d %s", method, path, r.Code, r.Body)
	}
	return r.Body
}
