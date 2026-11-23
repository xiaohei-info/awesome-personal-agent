package attention

// 定时任务产出注入主会话（§7.4 主动提醒）：简报、复盘、例行任务的产出总是写回 APA（事项与简报页）；
// 本人为这类任务或这个实例打开了「注入主会话」时，再经主 Agent 的唤醒路由把产出送进主会话，且只走这一条路，
// 不再按 routes 发渠道（同一份内容不会既发渠道又注入）。关着时简报照旧按 routes.brief 走，例行任务只记在 APA。
// 主会话不能唤醒时简报退回原路由、例行任务落 Web 未读；领取失败与无人领取由投递队列退避重试、超限退回 Web 未读。

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// TaskID 是简报以外的定时任务产出（例行任务）的投递 id：一次产出只入队一次。
func TaskID(item string) string { return "task:" + item }

// 注入正文的长度取舍：全文不超过 fullRunes 就给全文，否则每节给标题与前 sectionLines 行（每行至多 lineRunes），另附全文位置。
const (
	fullRunes    = 1800
	sectionLines = 2
	lineRunes    = 120
)

// taskLabel 是写在注入内容开头的任务类别。
var taskLabel = map[string]string{"brief": "简报", "review": "复盘", "routine": "例行任务"}

// taskDone：例行任务等非简报的定时任务产出转 done 时，开了注入主会话就产出一条投递（一次产出只一次）；关着不产出（只记在 APA）。
// handled 为真表示已经处理（含已入队），调用方不再按普通事项作废它的投递。
func (a *Attention) taskDone(tx *store.Tx, pol policy.Policy, it domain.Item) (bool, error) {
	_, on, err := instances.PushToMain(tx, pol, it)
	if err != nil || !on {
		return false, err
	}
	id := TaskID(it.ID)
	if known, err := a.known(tx, id); err != nil || known {
		return true, err
	}
	if err := a.drop(tx, it.ID); err != nil {
		return true, err
	}
	ver, err := command.Version(tx, it.ID)
	if err != nil {
		return true, err
	}
	return true, a.offer(tx, pol, held{id: id, item: it.ID, class: domain.DeliveryReminder, priority: it.Priority, version: ver,
		content: domain.DeliveryContent{Title: it.Title, WebURL: a.o.WebURL(it.ID)}})
}

// taskRoute 在写投递前改定时任务产出的路由与内容：开了注入主会话、且主会话能唤醒时，路由取主会话，内容换成带类别的正文。
// 在放行时判（而不是入队时），所以静默期间改了开关也按放行那一刻的设置走。
func (a *Attention) taskRoute(tx *store.Tx, pol policy.Policy, d *domain.Delivery) error {
	if d.Class != domain.DeliveryBrief && !strings.HasPrefix(d.ID, "task:") {
		return nil
	}
	it, err := items.Get(tx, d.Item)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	pack, on, err := instances.PushToMain(tx, pol, it)
	if err != nil || !on {
		return err
	}
	route, blocked, err := MainRoute(tx, pol)
	if err != nil {
		return err
	}
	if blocked != "" {
		if d.Class != domain.DeliveryBrief { // 例行任务没有原路由：落 Web 未读，能看到
			d.Route = delivery.RouteWeb
		}
		return nil
	}
	d.Route = route
	d.Content = TaskContent(it, pack, a.o.WebURL(it.ID))
	return nil
}

// TaskContent 是注入主会话的产出内容：标题带类别（简报、复盘、例行任务），正文按长度取全文或每节摘要。
func TaskContent(it domain.Item, pack, webURL string) domain.DeliveryContent {
	label := taskLabel[pack]
	if label == "" {
		label = "定时任务"
	}
	var spec struct {
		Sections []struct {
			Project string `json:"project"`
			Title   string `json:"title"`
			Body    string `json:"body"`
		} `json:"sections"`
		Result string `json:"result"`
	}
	_ = json.Unmarshal(it.Spec, &spec) // spec 不合预期时只用摘要，不挡投递
	type part struct{ head, body string }
	var parts []part
	for _, s := range spec.Sections {
		parts = append(parts, part{head: firstNonEmpty(s.Title, s.Project), body: strings.TrimSpace(s.Body)})
	}
	if len(parts) == 0 {
		if body := firstNonEmpty(strings.TrimSpace(spec.Result), strings.TrimSpace(it.Summary), strings.TrimSpace(it.Body)); body != "" {
			parts = append(parts, part{body: body})
		}
	}
	render := func(short bool) string {
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			var rows []string
			if p.head != "" {
				rows = append(rows, "## "+p.head)
			}
			if !short {
				out = append(out, strings.Join(append(rows, p.body), "\n"))
				continue
			}
			var lines []string
			for _, l := range strings.Split(p.body, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					lines = append(lines, l)
				}
			}
			for i, l := range lines {
				if i == sectionLines {
					rows = append(rows, "……（本节共 "+strconv.Itoa(len(lines))+" 行）")
					break
				}
				if r := []rune(l); len(r) > lineRunes {
					l = string(r[:lineRunes]) + "…"
				}
				rows = append(rows, l)
			}
			out = append(out, strings.Join(rows, "\n"))
		}
		return strings.TrimSpace(strings.Join(out, "\n\n"))
	}
	text := render(false)
	if utf8.RuneCountInString(text) > fullRunes {
		text = render(true) + "\n\n（正文较长，以上是摘要；全文在 APA 的这件事项里：" + it.ID + "）"
	}
	return domain.DeliveryContent{Title: "【" + label + "】" + it.Title, Summary: text, WebURL: webURL}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
