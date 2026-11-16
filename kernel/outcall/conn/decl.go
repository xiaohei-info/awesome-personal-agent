package conn

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
)

// Text 是中英两份文字；两份都必填。
type Text struct {
	Zh string `yaml:"zh" json:"zh"`
	En string `yaml:"en" json:"en"`
}

// 字段种类（§1.3）。
const (
	KindText     = "text"
	KindURL      = "url"
	KindNumber   = "number"
	KindSelect   = "select"
	KindToggle   = "toggle"
	KindSecret   = "secret"
	KindIdentify = "identify"
)

var kinds = map[string]bool{KindText: true, KindURL: true, KindNumber: true, KindSelect: true, KindToggle: true, KindSecret: true, KindIdentify: true}

// Choice 是 select 字段的一个选项。
type Choice struct {
	Value string `yaml:"value" json:"value"`
	Label Text   `yaml:"label" json:"label"`
}

// Field 是表单里的一项，自上而下就是界面顺序。
type Field struct {
	Key      string   `yaml:"key" json:"key"`
	Kind     string   `yaml:"kind" json:"kind"`
	Label    Text     `yaml:"label" json:"label"`
	Hint     *Text    `yaml:"hint,omitempty" json:"hint,omitempty"`
	Default  string   `yaml:"default,omitempty" json:"default,omitempty"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Group    string   `yaml:"group,omitempty" json:"group,omitempty"` // advanced 折叠
	Choices  []Choice `yaml:"choices,omitempty" json:"choices,omitempty"`
}

// Decl 是一种连接类型的接入声明（connector.yaml），构建期数据。
type Decl struct {
	Type         string       `yaml:"type" json:"type"`
	Direction    string       `yaml:"direction" json:"direction"`
	Title        Text         `yaml:"title" json:"title"`
	Tagline      Text         `yaml:"tagline" json:"tagline"`
	Default      Text         `yaml:"default" json:"default"` // 没给名字时的实例名
	Via          Text         `yaml:"via" json:"via"`         // 内容经过谁（§10.2 第 6 项）
	Capabilities Capabilities `yaml:"capabilities" json:"capabilities"`
	Fields       []Field      `yaml:"fields" json:"fields"`
	Guide        []Text       `yaml:"guide,omitempty" json:"guide,omitempty"`
}

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

func (t Text) ok() bool { return t.Zh != "" && t.En != "" }

// Check 校验声明本身；不合法是构建缺陷。
func (d Decl) Check() error {
	switch {
	case !keyRe.MatchString(d.Type):
		return fmt.Errorf("connector type %q is not a valid identifier", d.Type)
	case d.Direction != DirectionChannel && d.Direction != DirectionOutput:
		return fmt.Errorf("connector %s: direction %q", d.Type, d.Direction)
	case !d.Title.ok() || !d.Tagline.ok() || !d.Default.ok() || !d.Via.ok():
		return fmt.Errorf("connector %s: title, tagline, default and via need both zh and en", d.Type)
	case len(d.Guide) > 5:
		return fmt.Errorf("connector %s: guide has more than five steps", d.Type)
	}
	for _, g := range d.Guide {
		if !g.ok() {
			return fmt.Errorf("connector %s: guide step needs both zh and en", d.Type)
		}
	}
	seen := map[string]bool{}
	for _, f := range d.Fields {
		switch {
		case !keyRe.MatchString(f.Key) || seen[f.Key]:
			return fmt.Errorf("connector %s: field key %q invalid or repeated", d.Type, f.Key)
		case !kinds[f.Kind]:
			return fmt.Errorf("connector %s: field %s kind %q", d.Type, f.Key, f.Kind)
		case !f.Label.ok() || (f.Hint != nil && !f.Hint.ok()):
			return fmt.Errorf("connector %s: field %s label and hint need both zh and en", d.Type, f.Key)
		case (f.Kind == KindSelect) != (len(f.Choices) > 0):
			return fmt.Errorf("connector %s: field %s: choices only and always for select", d.Type, f.Key)
		case f.Kind == KindSecret && f.Default != "":
			return fmt.Errorf("connector %s: secret field %s cannot have a default", d.Type, f.Key)
		case f.Group != "" && f.Group != "advanced":
			return fmt.Errorf("connector %s: field %s group %q", d.Type, f.Key, f.Group)
		}
		if f.Default != "" {
			if err := f.check(f.Default); err != nil {
				return fmt.Errorf("connector %s: field %s default: %w", d.Type, f.Key, err)
			}
		}
		seen[f.Key] = true
	}
	return nil
}

// SecretKeys 是声明里的凭据字段。
func (d Decl) SecretKeys() []string {
	var out []string
	for _, f := range d.Fields {
		if f.Kind == KindSecret {
			out = append(out, f.Key)
		}
	}
	return out
}

// Field 按键找字段。
func (d Decl) Field(key string) (Field, bool) {
	for _, f := range d.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// check 校验一个非空取值是否符合字段种类。
func (f Field) check(v string) error {
	switch f.Kind {
	case KindURL:
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return fmt.Errorf("not an http(s) address without credentials")
		}
	case KindNumber:
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return fmt.Errorf("not a number")
		}
	case KindToggle:
		if v != "true" && v != "false" {
			return fmt.Errorf("not true or false")
		}
	case KindSelect:
		for _, c := range f.Choices {
			if c.Value == v {
				return nil
			}
		}
		return fmt.Errorf("not one of the choices")
	}
	if len(v) > 2048 {
		return fmt.Errorf("too long")
	}
	return nil
}

// FieldError 是配置不符合声明的错误：Field 是出错的字段键，供界面就地标出。
type FieldError struct{ Field, Why string }

func (e *FieldError) Error() string { return "field " + e.Field + ": " + e.Why }

// Normalize 按声明校验并规整非凭据配置：未知键与凭据键拒绝，缺省的取默认值；必填字段（含凭据）由 have 判断是否已有值。
func (d Decl) Normalize(settings map[string]string, haveSecret func(key string) bool) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range settings {
		f, ok := d.Field(k)
		if !ok || f.Kind == KindSecret {
			return nil, &FieldError{k, "not a setting of this connection type"}
		}
		if v == "" {
			continue
		}
		if err := f.check(v); err != nil {
			return nil, &FieldError{k, err.Error()}
		}
		out[k] = v
	}
	for _, f := range d.Fields {
		if f.Kind == KindSecret {
			if f.Required && !haveSecret(f.Key) {
				return nil, &FieldError{f.Key, "required"}
			}
			continue
		}
		if _, ok := out[f.Key]; !ok && f.Default != "" {
			out[f.Key] = f.Default
		}
		// identify 字段由识别流程写入，保存时可以为空（§2.6）；没填时发送返回 recipient_missing。
		if f.Required && f.Kind != KindIdentify && out[f.Key] == "" {
			return nil, &FieldError{f.Key, "required"}
		}
	}
	return out, nil
}
