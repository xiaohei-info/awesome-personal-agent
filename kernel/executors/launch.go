package executors

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 执行者参数与可覆盖参数（附录 A.2）：同一份声明承载多个执行者，执行者之间的差别（profile、所在机器）是
// executor_args，从执行者句柄取值；模型、思考档这类可以按执行者或按次换掉的是 overrides，只能取声明列出的值。
// 两者都只把结构化取值拼成 [标志, 值]，不接受自由字符串，命令本身只来自本人确认的声明。

// Override 是一项可覆盖参数。Role 标明它是模型（model）还是思考档（thinking），实际生效的值据此写进运行记录。
type Override struct {
	Label   Label    `json:"label"`
	Args    []string `json:"args"`
	Choices []string `json:"choices"`
	Role    string   `json:"role,omitempty"`
}

// 可覆盖参数的角色。
const (
	RoleModel    = "model"
	RoleThinking = "thinking"
)

func argsShape(key string, args []string) bool {
	return len(args) == 2 && optionFlag.MatchString(args[0]) && args[1] == "{"+key+"}"
}

func (d Declaration) validateArgs() error {
	if (len(d.ExecutorArgs) > 0 || len(d.Overrides) > 0) && d.Headless == nil {
		return invalid("executor_args and overrides need a headless declaration")
	}
	for key, args := range d.ExecutorArgs {
		if !handleKey.MatchString(key) || !argsShape(key, args) {
			return invalid("executor_args.%s must be [\"--flag\", \"{%s}\"]", key, key)
		}
	}
	roles := map[string]bool{}
	for key, o := range d.Overrides {
		if !fieldName.MatchString(key) || sensitive.MatchString(key) || !argsShape(key, o.Args) || !o.Label.valid() {
			return invalid("overrides.%s needs label {zh, en} and args [\"--flag\", \"{%s}\"]", key, key)
		}
		if _, clash := d.ExecutorArgs[key]; clash {
			return invalid("overrides.%s is also an executor_args key", key)
		}
		if len(o.Choices) == 0 {
			return invalid("overrides.%s must list its allowed values in choices", key)
		}
		for _, c := range o.Choices {
			if !optionValue.MatchString(c) {
				return invalid("overrides.%s has an invalid choice", key)
			}
		}
		if o.Role != "" {
			if (o.Role != RoleModel && o.Role != RoleThinking) || roles[o.Role] {
				return invalid("overrides.%s: role must be model or thinking, each at most once", key)
			}
			roles[o.Role] = true
		}
	}
	if c := d.Headless; c != nil && c.Cancel != nil {
		for _, key := range c.Cancel.ExecutorArgs {
			if _, ok := d.ExecutorArgs[key]; !ok {
				return invalid("headless.cancel.executor_args: %s is not an executor_args key", key)
			}
		}
	}
	return nil
}

// CheckOverrides 校验一组覆盖值：键必须是声明的可覆盖参数，值必须在它的 choices 里；空值表示不覆盖。
func (d Declaration) CheckOverrides(values map[string]string) error {
	for key, v := range values {
		o, ok := d.Overrides[key]
		if !ok {
			return invalid("%s is not an overridable parameter of %s", key, d.Harness)
		}
		if v != "" && !slices.Contains(o.Choices, v) {
			return invalid("%s=%s is not one of the values the declaration allows", key, v)
		}
	}
	return nil
}

// Launch 是一次拉起里与执行者有关的参数：执行者参数、生效的覆盖参数，以及据角色认出的模型与思考档。
type Launch struct {
	Args      []string          // 追加到命令后的执行者参数与覆盖参数
	Effective map[string]string // 生效的覆盖值（单次委派 > 执行者默认 > 不传）
	Model     string
	Thinking  string
}

// handleValues 取执行者句柄顶层的字符串字段（executor_args 只认这些）。
func handleValues(h json.RawMessage) map[string]string {
	var raw map[string]any
	_ = json.Unmarshal(h, &raw) // 句柄登记时已校验为对象
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// ExecutorArgv 把 keys（空表示全部）对应的执行者参数按键名排序拼出；句柄里没有、或取值不是单个结构化取值的字段不追加。
func (d Declaration) ExecutorArgv(e Entry, keys []string) []string {
	vals := handleValues(e.Handle)
	if keys == nil {
		keys = slices.Sorted(maps.Keys(d.ExecutorArgs))
	}
	var out []string
	for _, key := range keys {
		args, ok := d.ExecutorArgs[key]
		if v := vals[key]; ok && optionValue.MatchString(v) {
			out = append(out, args[0], v)
		}
	}
	return out
}

// LaunchFor 算出执行者 e 这一次的参数：once 是单次委派的覆盖，先于本人给执行者定的默认值；都没有的键不传参。
// once 里的值不在声明的 choices 里时报错（调用方按请求错误交回），执行者默认值在设置时已校验，声明改了之后不再合法的跳过。
func (d Declaration) LaunchFor(e Entry, once map[string]string) (Launch, error) {
	if err := d.CheckOverrides(once); err != nil {
		return Launch{}, err
	}
	l := Launch{Args: d.ExecutorArgv(e, nil), Effective: map[string]string{}}
	for _, key := range slices.Sorted(maps.Keys(d.Overrides)) {
		o := d.Overrides[key]
		v := once[key]
		if v == "" {
			v = e.Overrides[key]
		}
		if v == "" || !slices.Contains(o.Choices, v) {
			continue
		}
		l.Args = append(l.Args, o.Args[0], v)
		l.Effective[key] = v
		switch o.Role {
		case RoleModel:
			l.Model = v
		case RoleThinking:
			l.Thinking = v
		}
	}
	return l, nil
}

// ExecutorArgsMarker 是命令模板里放执行者参数与覆盖参数的位置（整个元素）：有的宿主要求它们在子命令之前
// （如 `<程序> -p <profile> chat …`）；模板里没有它时这些参数追加在命令之后。
const ExecutorArgsMarker = "{executor_args}"

// Expand 展开命令模板：vals 是占位符到取值（{prompt}、{run_dir}、{run_context}、{run_id}、{report_schema}），
// extra 是执行者参数与覆盖参数，放在 {executor_args} 处，模板里没有这个位置就追加在后面。不认识的占位符在登记时已拒绝。
func Expand(cmd []string, vals map[string]string, extra []string) []string {
	pairs := make([]string, 0, 2*len(vals))
	for k, v := range vals {
		pairs = append(pairs, k, v)
	}
	r := strings.NewReplacer(pairs...)
	out, spliced := make([]string, 0, len(cmd)+len(extra)), false
	for _, a := range cmd {
		if a == ExecutorArgsMarker {
			out, spliced = append(out, extra...), true
			continue
		}
		out = append(out, r.Replace(a))
	}
	if !spliced {
		out = append(out, extra...)
	}
	return out
}

// CancelArgv 是这次运行的取消命令；声明没有 cancel 时为空。
func (h Headless) CancelArgv(d Declaration, e Entry, runID, sessionID string) []string {
	if h.Cancel == nil {
		return nil
	}
	r := strings.NewReplacer("{run_id}", runID, "{session_id}", sessionID)
	out := make([]string, 0, len(h.Cancel.Command))
	for _, a := range h.Cancel.Command {
		out = append(out, r.Replace(a))
	}
	keys := h.Cancel.ExecutorArgs
	if keys == nil {
		keys = []string{}
	}
	return append(out, d.ExecutorArgv(e, keys)...)
}

// ---- 本人给执行者定默认值（executor.override） ----

type overrideIn struct {
	ID     string            `json:"id"`
	Values map[string]string `json:"values"`
}

// override 是本人在设置页给执行者改可覆盖参数的默认值：只能取执行者所属声明列出的值，空值表示不覆盖（沿用声明或执行者自带的默认）。
func override(c *command.Ctx) (any, error) {
	var in overrideIn
	if err := decode(c.Input, &in); err != nil {
		return nil, err
	}
	e, ok, err := Get(c.Tx, in.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("executor " + in.ID)
	}
	rec, ok, err := record(c.Tx, e.Harness, 0, StateActive)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, invalid("executor %s has no confirmed declaration to take overridable parameters from", in.ID)
	}
	if err := rec.Declaration.CheckOverrides(in.Values); err != nil {
		return nil, err
	}
	vals := map[string]string{}
	for k, v := range in.Values {
		if v != "" {
			vals[k] = v
		}
	}
	doc := ""
	if len(vals) > 0 {
		doc = string(payload(vals))
	}
	if _, err := c.Tx.Exec("UPDATE executors_entry SET overrides = ? WHERE id = ?", doc, in.ID); err != nil {
		return nil, err
	}
	e.Overrides = vals
	_, err = c.Emit(domain.Event{Type: EvChanged, Object: c.Object.ID, Payload: payload(e)})
	return e, err
}

// Overridable 返回执行者所属声明的可覆盖参数（供 executor.list 与 Web 展示可选值）；没有生效声明为空。
func Overridable(tx *store.Tx, harness string) (map[string]Override, error) {
	rec, ok, err := record(tx, harness, 0, StateActive)
	if err != nil || !ok {
		return nil, err
	}
	return rec.Declaration.Overrides, nil
}
