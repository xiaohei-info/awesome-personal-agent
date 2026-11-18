package executors

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type Card struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Harness string            `json:"harness"`
	Values  map[string]string `json:"values,omitempty"`
	Enabled bool              `json:"enabled"`
	Rev     int64             `json:"rev"`
}

const cardColumns = `SELECT id,harness,name,options,enabled,rev FROM executors_agent`

func readCard(rows *store.Rows) (card Card, err error) {
	var options string
	if err = rows.Scan(&card.ID, &card.Harness, &card.Name, &options, &card.Enabled, &card.Rev); err == nil {
		err = json.Unmarshal([]byte(options), &card.Values)
	}
	return
}

func StoredCard(tx *store.Tx, id string) (card Card, err error) {
	err = query(tx, func(rows *store.Rows) error { card, err = readCard(rows); return err }, cardColumns+` WHERE id=?`, id)
	return
}

func storedCards(tx *store.Tx, harness string) ([]Card, error) {
	cards := []Card{}
	err := query(tx, func(rows *store.Rows) error {
		card, err := readCard(rows)
		if err == nil {
			cards = append(cards, card)
		}
		return err
	}, cardColumns+` WHERE (?='' OR harness=?) ORDER BY name,id`, harness, harness)
	return cards, err
}

func legacyCard(harness string, rec Record) Card {
	name := harness
	if rec.Declaration.Name != nil {
		name = rec.Declaration.Name.Zh
	}
	return Card{ID: "legacy:" + harness, Harness: harness, Name: name, Enabled: true}
}

func Cards(tx *store.Tx) ([]Card, error) {
	cards, err := storedCards(tx, "")
	if err != nil {
		return nil, err
	}
	records, err := Records(tx, StateActive)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		cards = append(cards, legacyCard(rec.Harness, rec))
	}
	return cards, nil
}

func saveCard(c *command.Ctx) (any, error) {
	var card Card
	if err := decode(c.Input, &card); err != nil {
		return nil, err
	}
	card.Name = strings.TrimSpace(card.Name)
	if card.Name == "" || len([]rune(card.Name)) > 80 {
		return nil, invalid("Agent 名字须为 1 至 80 个字")
	}
	rec, ok, err := record(c.Tx, card.Harness, 0, StateActive)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, invalid("宿主声明尚未确认")
	}
	if err := rec.Declaration.InstanceOptions.check(card.Values); err != nil {
		return nil, err
	}
	values := string(payload(card.Values))
	if card.ID == "" {
		card.ID = "agent_" + rand.Text()
		card.Rev = 1
		_, err = c.Tx.Exec(`INSERT INTO executors_agent (id,harness,name,options,enabled,rev) VALUES(?,?,?,?,?,1)`, card.ID, card.Harness, card.Name, values, card.Enabled)
	} else {
		var changed int64
		changed, err = c.Tx.Exec(`UPDATE executors_agent SET name=?,options=?,enabled=?,rev=rev+1 WHERE id=? AND harness=? AND rev=?`, card.Name, values, card.Enabled, card.ID, card.Harness, card.Rev)
		if err == nil && changed != 1 {
			return nil, conflict("卡片已变更，请重新载入")
		}
		card.Rev++
	}
	if err != nil {
		return nil, err
	}
	_, err = c.Emit(domain.Event{Type: domain.EvAgentConfigured, Object: card.ID, Payload: payload(card)})
	return card, err
}

func CheckRun(tx *store.Tx, run domain.Run) error {
	if run.AgentSnapshot == nil {
		return nil
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	selected, err := Resolve(tx, pol, Selection{Item: run.AgentID, Runner: run.Runner})
	if err != nil {
		return err
	}
	if reason := selected.AccessReason(run.Access); reason != "" {
		return conflict("%s", reason)
	}
	if selected.Record.Rev != run.DeclarationRev {
		return conflict("启动声明已变更，请重新运行")
	}
	return nil
}

type Selection struct {
	Item, Schedule, Rule, Harness, Runner string
}
type Selected struct {
	Card     Card
	Record   Record
	Snapshot domain.AgentSnapshot
	Args     []string
	Env      map[string]string
	Reason   string
}

func (s Selected) AccessReason(request string) string {
	if s.Reason != "" {
		return s.Reason
	}
	ranks := map[string]int{"read": 1, "edit": 2, "full": 3}
	if _, ok := s.Record.Declaration.Headless.Access[request]; !ok {
		return "无头运行声明未提供权限档位：" + request
	}
	if ranks[request] == 0 || ranks[request] > ranks[s.Snapshot.PermissionCap] {
		return fmt.Sprintf("权限档位超过 Web 设置的上限：需要 %s，%s 只能 %s", request, s.Card.Name, s.Snapshot.PermissionCap)
	}
	return ""
}

func Resolve(tx *store.Tx, pol policy.Policy, sel Selection) (result Selected, err error) {
	id, source := sel.Item, "item"
	if id == "" {
		id, source = sel.Schedule, "schedule"
	}
	if id == "" {
		id, source = pol.RuleAgent[sel.Rule], "rule"
	}
	if id == "" && sel.Harness != "" {
		id, source = "legacy:"+sel.Harness, "rule"
	}
	if id == "" {
		role := pol.DefaultAgentID
		source = "default"
		if sel.Runner == "main_session" {
			role = pol.MainAgentID
			source = "main"
		}
		if role != nil {
			id = *role
		} else if pol.DefaultHarness != "" {
			id = "legacy:" + pol.DefaultHarness
		}
	}
	if id == "" {
		result.Reason = "还没选默认 Agent"
		if source == "main" {
			result.Reason = "还没选主 Agent"
		}
		if pol.DefaultAgentID == nil && pol.MainAgentID == nil {
			result.Reason = "规则没有指定 harness，Web 设置里也没有默认 harness"
		}
		return
	}
	var card Card
	harness, legacy := strings.CutPrefix(id, "legacy:")
	if !legacy {
		card, err = StoredCard(tx, id)
		if err != nil {
			return result, err
		}
		harness = card.Harness
	}
	rec, ok, err := record(tx, harness, 0, StateActive)
	if err != nil {
		return result, err
	}
	if legacy {
		card = legacyCard(harness, rec)
	}
	result.Card, result.Record = card, rec
	result.Snapshot = domain.AgentSnapshot{AgentID: id, AgentName: card.Name, ConfigRev: card.Rev, DeclarationRev: rec.Rev, SelectionSource: source}
	prefix := map[string]string{"item": "事项", "schedule": "安排", "rule": "规则", "default": "日常角色", "main": "主角色"}[source] + "选择的 Agent "
	switch {
	case card.ID == "":
		result.Reason = prefix + id + " 不存在"
	case !card.Enabled:
		result.Reason = prefix + card.Name + " 已停用"
	case !ok || rec.Declaration.Headless == nil:
		result.Reason = prefix + card.Name + " 没有已确认的无头运行声明"
	case !legacy && sel.Runner == "main_session" && rec.Declaration.MainSession == nil:
		result.Reason = prefix + card.Name + " 不支持主角色"
	}
	if result.Reason != "" {
		return
	}
	cap := pol.PermissionCap.ByAgent[id]
	if cap == "" && legacy {
		cap = pol.PermissionCap.ByHarness[harness]
		if cap == "" {
			cap = pol.PermissionCap.Default
		}
	}
	if cap == "" {
		cap = policy.PermRead
	}
	var model string
	result.Args, result.Env, model = rec.Declaration.InstanceOptions.expand(card.Values)
	result.Snapshot.PermissionCap, result.Snapshot.ConfiguredModel = cap, model
	return
}
