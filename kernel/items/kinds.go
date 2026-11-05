package items

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// Kinds 登记 kind 专属 spec 的 JSON Schema（§6.3）：登记了就校验，未登记则 spec 原样存取。
// 能力包加载器在加载时调用 Register；本包不认识任何具体 kind。
//
// shortcut: 只支持 type、enum、required、properties、additionalProperties(bool)、items 子集，其余关键字在
// 登记时被拒绝（不静默忽略）；需要 pattern、范围、$ref 等时再扩展，不引入第三方校验库。
type Kinds struct {
	mu      sync.RWMutex
	schemas map[domain.Kind]map[string]any
}

func NewKinds() *Kinds { return &Kinds{schemas: map[domain.Kind]map[string]any{}} }

var annotations = map[string]bool{"$schema": true, "title": true, "description": true}

var schemaTypes = map[string]bool{"string": true, "number": true, "integer": true, "boolean": true, "object": true, "array": true, "null": true}

// Register 登记或替换 kind 的 spec schema；不受支持的关键字报错。
func (k *Kinds) Register(kind domain.Kind, schema json.RawMessage) error {
	if strings.TrimSpace(string(kind)) == "" {
		return fmt.Errorf("items: kind is empty")
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil || s == nil {
		return fmt.Errorf("items: schema for kind %q is not a JSON object", kind)
	}
	if err := checkSchema(s, "schema"); err != nil {
		return fmt.Errorf("items: kind %q: %w", kind, err)
	}
	k.mu.Lock()
	k.schemas[kind] = s
	k.mu.Unlock()
	return nil
}

// CheckSchema 只检查 schema 是否在受支持的子集内，不登记；能力包加载器用它在登记前整体预检。
func (k *Kinds) CheckSchema(schema json.RawMessage) error {
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil || s == nil {
		return fmt.Errorf("items: schema is not a JSON object")
	}
	return checkSchema(s, "schema")
}

func checkSchema(s map[string]any, path string) error {
	for key, v := range s {
		switch key {
		case "type":
			t, _ := v.(string)
			if !schemaTypes[t] {
				return fmt.Errorf("%s: unsupported type %v", path, v)
			}
		case "enum":
			if _, ok := v.([]any); !ok {
				return fmt.Errorf("%s: enum must be a list", path)
			}
		case "required":
			l, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%s: required must be a list", path)
			}
			for _, r := range l {
				if _, ok := r.(string); !ok {
					return fmt.Errorf("%s: required must list strings", path)
				}
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: properties must be an object", path)
			}
			for name, p := range props {
				sub, ok := p.(map[string]any)
				if !ok {
					return fmt.Errorf("%s.%s: schema must be an object", path, name)
				}
				if err := checkSchema(sub, path+"."+name); err != nil {
					return err
				}
			}
		case "additionalProperties":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s: additionalProperties must be a boolean", path)
			}
		case "items":
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: items must be a schema object", path)
			}
			if err := checkSchema(sub, path+"[]"); err != nil {
				return err
			}
		default:
			if !annotations[key] {
				return fmt.Errorf("%s: unsupported keyword %q", path, key)
			}
		}
	}
	return nil
}

// check 按 kind 的 schema 校验 spec；未登记（或 k 为 nil）直接通过。省略 spec 按空对象校验，
// 所以带 required 的 kind 必须给 spec。
func (k *Kinds) check(kind domain.Kind, spec json.RawMessage) error {
	if k == nil {
		return nil
	}
	k.mu.RLock()
	s, ok := k.schemas[kind]
	k.mu.RUnlock()
	if !ok {
		return nil
	}
	if len(bytes.TrimSpace(spec)) == 0 {
		spec = json.RawMessage(`{}`)
	}
	var v any
	if err := json.Unmarshal(spec, &v); err != nil {
		return invalid("spec is not valid JSON")
	}
	if err := validate(s, v, "spec"); err != nil {
		return invalid("spec does not match the schema of kind " + string(kind) + ": " + err.Error())
	}
	return nil
}

func validate(s map[string]any, v any, path string) error {
	if t, ok := s["type"].(string); ok && !typeMatches(t, v) {
		return fmt.Errorf("%s: want type %s", path, t)
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if reflect.DeepEqual(x, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: not in enum", path)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, has := x[r.(string)]; !has {
					return fmt.Errorf("%s: missing required %q", path, r)
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for name, val := range x {
			sub, declared := props[name].(map[string]any)
			if !declared {
				if ap, ok := s["additionalProperties"].(bool); ok && !ap {
					return fmt.Errorf("%s: unexpected property %q", path, name)
				}
				continue
			}
			if err := validate(sub, val, path+"."+name); err != nil {
				return err
			}
		}
	case []any:
		if sub, ok := s["items"].(map[string]any); ok {
			for i, el := range x {
				if err := validate(sub, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func typeMatches(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "null":
		return v == nil
	}
	return false
}

// ValidateAgainst 按同一 schema 子集校验任意 JSON 值（已解码）；能力包实例参数复用它，不再造第二份校验器。
// schema 须已通过 CheckSchema。
func ValidateAgainst(schema map[string]any, v any) error { return validate(schema, v, "params") }
