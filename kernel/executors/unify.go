package executors

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Unified 是执行者统一切换（§16.5）在本模块里做的事：旧 Agent 卡换成隔离另写的声明与执行者，被选用的兼容卡换成执行者。
type Unified struct {
	// Map 是旧 Agent id（卡 id 或 legacy:<程序>）到执行者 id；选人的位置据它换算，不在表里的清空。
	Map map[string]string `json:"map"`
	// Declarations 是隔离另写的声明（键是原卡 id，权限卡、钥匙绑定与审计里的 Agent 因此不用改写）。
	Declarations []string `json:"declarations,omitempty"`
	// Executors 是新建的执行者。
	Executors []string `json:"executors,omitempty"`
	// Skipped 是没迁的旧卡 → 原因；Dropped 是卡上换算不成执行者参数的取值（字段 → 原因）。
	Skipped map[string]string `json:"skipped,omitempty"`
	Dropped map[string]string `json:"dropped,omitempty"`
}

var slugBad = regexp.MustCompile(`[^a-z0-9._-]+`)

// Unify 做本模块那一步，在切换的事务里调用、只调一次（切换据 authz.switched 判定做没做过）：
//   - 每张启用的旧卡：复制它所属程序的生效声明，键换成卡 id、base 记程序、名字取卡名、修订号沿用（排队与可续接的运行
//     复核声明修订时不因切换失败）；卡上按 args_if_set 拼参数的取值换成可覆盖参数（取值只列卡上现有的值），
//     instance_options 去掉；再建一个执行者（id 取卡名的合法写法，冲突时用卡 id），默认值就是卡上的值。拉起命令与切换前一致。
//   - 每份生效的程序声明：referenced 里有它的兼容卡 legacy:<程序>，或它下面还没有任何执行者时，建一个 id 为程序名的执行者。
func Unify(tx *store.Tx, referenced []string) (Unified, error) {
	u := Unified{Map: map[string]string{}, Skipped: map[string]string{}, Dropped: map[string]string{}}
	cards, err := LegacyCards(tx)
	if err != nil {
		return u, err
	}
	for _, c := range cards {
		if !c.Enabled {
			u.Skipped[c.ID] = "disabled"
			continue
		}
		rec, ok, err := record(tx, c.Harness, 0, StateActive)
		if err != nil {
			return u, err
		}
		if !ok || rec.Declaration.Headless == nil {
			u.Skipped[c.ID] = "no confirmed headless declaration for " + c.Harness
			continue
		}
		d, overrides, err := separate(rec.Declaration, c, u.Dropped)
		if err != nil {
			return u, err
		}
		doc, err := json.Marshal(d)
		if err != nil {
			return u, err
		}
		if _, err := tx.Exec("INSERT INTO executors_declaration (harness, rev, doc, state) VALUES (?, ?, ?, ?)", d.Harness, rec.Rev, string(doc), StateActive); err != nil {
			return u, err
		}
		u.Declarations = append(u.Declarations, d.Harness)
		id, err := freeID(tx, slug(c.Name), c.ID)
		if err != nil {
			return u, err
		}
		if err := newEntry(tx, Executor{ID: id, Kind: KindExternalCLI, Label: c.Name}, d.Harness, overrides, c.ID); err != nil {
			return u, err
		}
		u.Map[c.ID], u.Executors = id, append(u.Executors, id)
	}
	recs, err := Records(tx, StateActive)
	if err != nil {
		return u, err
	}
	es, err := List(tx)
	if err != nil {
		return u, err
	}
	for _, rec := range recs {
		h := rec.Harness
		if rec.Declaration.Base != "" || rec.Declaration.Headless == nil {
			continue
		}
		has := slices.ContainsFunc(es, func(e Entry) bool { return e.Harness == h })
		if has && !slices.Contains(referenced, "legacy:"+h) {
			continue
		}
		if e, ok, err := Get(tx, h); err != nil {
			return u, err
		} else if ok {
			if e.Harness == h {
				u.Map["legacy:"+h] = h
				if _, err := tx.Exec("UPDATE executors_entry SET legacy_agent = ? WHERE id = ?", "legacy:"+h, h); err != nil {
					return u, err
				}
			} else {
				u.Skipped["legacy:"+h] = "executor id " + h + " belongs to " + e.Harness
			}
			continue
		}
		if err := newEntry(tx, Executor{ID: h, Kind: KindExternalCLI, Label: rec.Declaration.DisplayName()}, h, nil, "legacy:"+h); err != nil {
			return u, err
		}
		u.Map["legacy:"+h], u.Executors = h, append(u.Executors, h)
	}
	return u, nil
}

// separate 由程序的声明 base 与旧卡 c 得出隔离另写的声明与执行者默认值。
func separate(base Declaration, c LegacyCard, dropped map[string]string) (Declaration, map[string]string, error) {
	var d Declaration
	raw, err := json.Marshal(base)
	if err != nil {
		return d, nil, err
	}
	if err := json.Unmarshal(raw, &d); err != nil { // 深拷贝
		return d, nil, err
	}
	opts := d.InstanceOptions
	d.Harness, d.Base, d.Name, d.InstanceOptions, d.Executors, d.Selfcheck = c.ID, c.Harness, &Label{Zh: c.Name, En: c.Name}, nil, nil, nil
	vals := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(c.Values)) {
		v := c.Values[k]
		var args []string
		var field OptionField
		if opts != nil {
			args, field = opts.Args[k], opts.Fields[k]
		}
		if len(args) != 2 || !optionValue.MatchString(v) {
			dropped[c.ID+"."+k] = "no args_if_set flag for it (environment and working-directory options do not carry over)"
			continue
		}
		if d.Overrides == nil {
			d.Overrides = map[string]Override{}
		}
		o, ok := d.Overrides[k]
		if !ok {
			o = Override{Label: field.Label, Args: args}
			if field.DisplayAs == "model" {
				o.Role = RoleModel
			}
		}
		if !slices.Contains(o.Choices, v) {
			o.Choices = append(o.Choices, v)
		}
		d.Overrides[k], vals[k] = o, v
	}
	return d, vals, d.validate()
}

func slug(name string) string {
	return strings.Trim(slugBad.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-._")
}

// freeID 取 want（合法且没被占用时），否则 fallback。
func freeID(tx *store.Tx, want, fallback string) (string, error) {
	if !enrollID.MatchString(want) {
		return fallback, nil
	}
	if _, taken, err := Get(tx, want); err != nil || taken {
		return fallback, err
	}
	return want, nil
}

func newEntry(tx *store.Tx, e Executor, harness string, overrides map[string]string, legacy string) error {
	if err := putEntry(tx, e, harness); err != nil {
		return err
	}
	doc := ""
	if len(overrides) > 0 {
		doc = string(payload(overrides))
	}
	_, err := tx.Exec("UPDATE executors_entry SET overrides = ?, legacy_agent = ? WHERE id = ?", doc, legacy, e.ID)
	return err
}
