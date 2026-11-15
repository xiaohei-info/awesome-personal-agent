// Package sources 汇总仓库自带来源程序的接入声明（sources/<程序目录>/source.yaml，主文档 §9.1、附录 D.1）。
// 声明随 apa 主程序嵌入，发布物不另带文件；装配方据此给登记取显示名，内核不认识具体程序。
package sources

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed */source.yaml
var files embed.FS

// Text 是一个名字的中英两份；可含 {字段} 占位，取 config 里同名的字符串值。
type Text struct {
	Zh string `yaml:"zh" json:"zh"`
	En string `yaml:"en" json:"en"`
}

// Declaration 是一个来源程序的接入声明。
type Declaration struct {
	Program string `yaml:"program"` // 登记里 command 第一个词的文件名部分
	// WebhookSourceType 非空时，type=webhook、source_type 等于它的登记也按本声明取名（webhook 来源没有命令）。
	WebhookSourceType string          `yaml:"webhook_source_type"`
	Name              Text            `yaml:"name"`  // 名字模板；占位取不到值时用 Default
	By                string          `yaml:"by"`    // 按 config 的这个字段区分实例
	Names             map[string]Text `yaml:"names"` // By 字段的取值 → 名字模板，优先于 Name
	Default           Text            `yaml:"default"`
}

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

// Load 读出全部嵌入的声明；不合法的声明是构建缺陷，直接报错。
func Load() ([]Declaration, error) {
	names, err := fs.Glob(files, "*/source.yaml")
	if err != nil {
		return nil, err
	}
	var out []Declaration
	seen := map[string]bool{}
	for _, n := range names {
		b, err := files.ReadFile(n)
		if err != nil {
			return nil, err
		}
		var d Declaration
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&d); err != nil {
			return nil, fmt.Errorf("sources: %s: %w", n, err)
		}
		if d.Program == "" || seen[d.Program] {
			return nil, fmt.Errorf("sources: %s: program missing or declared twice", n)
		}
		if d.Default.Zh == "" || d.Default.En == "" || strings.Contains(d.Default.Zh+d.Default.En, "{") {
			return nil, fmt.Errorf("sources: %s: default needs zh and en without placeholders", n)
		}
		if (d.By == "") != (len(d.Names) == 0) {
			return nil, fmt.Errorf("sources: %s: by and names go together", n)
		}
		seen[d.Program] = true
		out = append(out, d)
	}
	return out, nil
}

// Find 返回与登记对应的声明：按 command 的程序名，webhook 按 source_type。
func Find(ds []Declaration, typ, command, sourceType string) (Declaration, bool) {
	prog := ""
	if f := strings.Fields(command); len(f) > 0 {
		prog = path.Base(strings.ReplaceAll(f[0], `\`, "/"))
	}
	for _, d := range ds {
		if (prog != "" && d.Program == prog) || (typ == "webhook" && d.WebhookSourceType != "" && d.WebhookSourceType == sourceType) {
			return d, true
		}
	}
	return Declaration{}, false
}

// NameFor 是声明给这份 config 的名字（lang 为 zh 或 en）：By 字段命中 Names 时用它，否则用 Name；
// 占位都取到非空字符串时展开，否则用 Default。
func (d Declaration) NameFor(config json.RawMessage, lang string) string {
	var cfg map[string]any
	_ = json.Unmarshal(config, &cfg) // config 由登记校验过；不是对象时当作没有字段
	pick := func(t Text) string {
		if lang == "en" {
			return t.En
		}
		return t.Zh
	}
	tpl := pick(d.Name)
	if v, ok := cfg[d.By].(string); ok && d.By != "" {
		if t, ok := d.Names[v]; ok {
			tpl = pick(t)
		}
	}
	ok := tpl != ""
	name := placeholder.ReplaceAllStringFunc(tpl, func(m string) string {
		v, _ := cfg[m[1:len(m)-1]].(string)
		if strings.TrimSpace(v) == "" {
			ok = false
		}
		return strings.TrimSpace(v)
	})
	if !ok {
		return pick(d.Default)
	}
	return name
}
