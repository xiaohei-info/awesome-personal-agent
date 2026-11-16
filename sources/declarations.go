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
	Sender            Sender          `yaml:"sender"`  // 发件人是否可信（§9.1），必填
	Content           Content         `yaml:"content"` // 原文取回能力（附录 D.8），可选
}

// Content 是接入声明的可选 content 段。
type Content struct {
	// Attachments 为 fetch 时，程序支持 fetch 模式：按清单里的句柄取回附件（主文档 §9.2）。不写即不支持。
	Attachments string `yaml:"attachments"`
}

// FetchesAttachments 报告程序是否声明了附件取回。
func (d Declaration) FetchesAttachments() bool { return d.Content.Attachments == AttachmentsFetch }

// AttachmentsFetch 是 content.attachments 唯一的取值。
const AttachmentsFetch = "fetch"

// 发件人信任的取值（§9.1）：声明的结论只有前两种；Types 可再把某类事件标为不带发件人。
const (
	TrustVerified   = "verified"   // 发件人由平台机制填写，可以核实
	TrustUnverified = "unverified" // 发件人由对方自己写，需确认
	TrustNone       = "none"       // 这类事件不带发件人
)

// Sender 是接入声明里的发件人段。Trust 是对本人显示的唯一结论；Types 按事件类型（可用 path.Match 通配）覆盖它，
// 精确的类型优先，其次是最长的匹配模式。
type Sender struct {
	Trust string            `yaml:"trust"`
	Types map[string]string `yaml:"types"`
	Hint  Text              `yaml:"hint"` // 可信时必填：信任发件人输入框的格式提示
	Self  string            `yaml:"self"` // identity：程序支持 identity 模式，能推出本人身份（§9.2）
}

// SenderFor 是这类事件的发件人上限：verified、unverified 或 none。
func (d Declaration) SenderFor(eventType string) string {
	if v, ok := d.Sender.Types[eventType]; ok {
		return v
	}
	best, out := -1, d.Sender.Trust
	for pat, v := range d.Sender.Types {
		if ok, _ := path.Match(pat, eventType); ok && len(pat) > best {
			best, out = len(pat), v
		}
	}
	return out
}

func validContent(c Content) error {
	if c.Attachments != "" && c.Attachments != AttachmentsFetch {
		return fmt.Errorf("content.attachments must be %q or empty, got %q", AttachmentsFetch, c.Attachments)
	}
	return nil
}

func validSender(s Sender) error {
	if s.Trust != TrustVerified && s.Trust != TrustUnverified {
		return fmt.Errorf("sender.trust must be verified or unverified")
	}
	if s.Trust == TrustVerified && (s.Hint.Zh == "" || s.Hint.En == "") {
		return fmt.Errorf("sender.hint needs zh and en when trust is verified")
	}
	if s.Self != "" && s.Self != "identity" {
		return fmt.Errorf("sender.self must be identity or empty")
	}
	for pat, v := range s.Types {
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("sender.types %q: %w", pat, err)
		}
		if v != TrustVerified && v != TrustUnverified && v != TrustNone {
			return fmt.Errorf("sender.types %q must be verified, unverified or none", pat)
		}
	}
	return nil
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
		if err := validContent(d.Content); err != nil {
			return nil, fmt.Errorf("sources: %s: %w", n, err)
		}
		if err := validSender(d.Sender); err != nil {
			return nil, fmt.Errorf("sources: %s: %w", n, err)
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
