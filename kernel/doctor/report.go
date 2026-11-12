// Package doctor 是 `apa doctor` 与 GET /doctor 共用的自检内核（设计 §15.5）：只读，每项给结论、机制性的原因和下一步，
// 不含账本里的用户内容（事项标题、正文、来源报错一律不进报告；只出现 id、计数、时间和机制说明）。
//
// 内核一侧的检查（运行形态、模型端点、来源、能力包、渠道与导出）只依赖账本事实和能力包加载器，GET /doctor 与 CLI 调同一份代码；
// 涉及本机文件与宿主的检查（数据目录、运行配置、宿主插件、重复提醒）在 cli/doctor 里做。
package doctor

// Status 是一项检查的结论。
type Status string

// 结论取值：ok 正常，warning 提醒，error 错误。
const (
	OK      Status = "ok"
	Warning Status = "warning"
	Error   Status = "error"
)

// 检查名（稳定标识，JSON 的 name），Order 给出输出顺序，与设计 §15.5 的表一致。
const (
	DataDir       = "data_dir"
	Config        = "config"
	Tier          = "tier"
	ModelEndpoint = "model_endpoint"
	Sources       = "sources"
	Packs         = "packs"
	HostPlugin    = "host_plugin"
	Overlaps      = "overlaps"
	Channels      = "channels"
)

// Order 是检查的输出顺序。
var Order = []string{DataDir, Config, Tier, ModelEndpoint, Sources, Packs, HostPlugin, Overlaps, Channels}

// Detail 是一项检查里针对某个对象（来源、端点、包、插件、渠道）的结论。
type Detail struct {
	Subject string         `json:"subject"`
	Status  Status         `json:"status"`
	Why     string         `json:"why"`
	Next    string         `json:"next,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// Check 是一项检查。Status 是 Details 里最差的一个；没有 Details 时由检查自己给。
type Check struct {
	Name    string         `json:"name"`
	Status  Status         `json:"status"`
	Why     string         `json:"why"`
	Next    string         `json:"next"`
	Details []Detail       `json:"details,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// Report 是自检结果。OK 为真表示没有错误级结论（退出码 0）；提醒不影响它。
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

func rank(s Status) int {
	switch s {
	case Error:
		return 2
	case Warning:
		return 1
	}
	return 0
}

// Worst 返回两个结论里更差的一个。
func Worst(a, b Status) Status {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Add 追加一个对象的结论，并把检查的结论抬到所有对象里最差的一个。
func (c *Check) Add(d Detail) {
	c.Details = append(c.Details, d)
	c.Status = Worst(c.Status, d.Status)
}

// Find 返回名为 subject 的对象结论。
func (c Check) Find(subject string) (*Detail, bool) {
	for i := range c.Details {
		if c.Details[i].Subject == subject {
			return &c.Details[i], true
		}
	}
	return nil, false
}

// Assemble 按 Order 排序，同名检查后者覆盖前者，并算出 OK。
func Assemble(checks ...Check) Report {
	byName := map[string]Check{}
	var extra []string // 不在 Order 里的（升级预检的专用项）按给出的顺序排在后面
	known := map[string]bool{}
	for _, n := range Order {
		known[n] = true
	}
	for _, c := range checks {
		if _, dup := byName[c.Name]; !dup && !known[c.Name] {
			extra = append(extra, c.Name)
		}
		byName[c.Name] = c
	}
	r := Report{OK: true, Checks: []Check{}}
	for _, n := range append(append([]string{}, Order...), extra...) {
		if c, ok := byName[n]; ok {
			r.Checks = append(r.Checks, c)
			if c.Status == Error {
				r.OK = false
			}
		}
	}
	return r
}
