package model

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
)

// validate 实现结构化输出用到的 JSON Schema 子集：type、enum、const、properties、required、
// additionalProperties(false)、items、minimum、maximum、minLength、maxLength、minItems、maxItems。
// 子集之外的关键字由 checkSchema 在发请求前拒绝（fail closed），不会被静默忽略。
// shortcut: 不支持 $ref/oneOf/anyOf/pattern；能力包需要时再扩展，不引入第三方校验库。
func validate(s map[string]any, v any, path string) error {
	// 模型输出不可信：超出 float64 范围的数字（如 1e999999999999）按违规处理。
	if n, ok := v.(json.Number); ok {
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil || math.IsInf(f, 0) {
			return fmt.Errorf("%s: number out of range", path)
		}
		v = f
	}
	if t, ok := s["type"]; ok {
		if !typeMatches(t, v) {
			return fmt.Errorf("%s: want type %v", path, t)
		}
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if equal(x, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: not in enum", path)
		}
	}
	if c, ok := s["const"]; ok && !equal(c, v) {
		return fmt.Errorf("%s: not const", path)
	}
	switch x := v.(type) {
	case map[string]any:
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if k, _ := r.(string); k != "" {
					if _, has := x[k]; !has {
						return fmt.Errorf("%s: missing required %q", path, k)
					}
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, val := range x {
			sub, declared := props[k].(map[string]any)
			if !declared {
				if ap, ok := s["additionalProperties"].(bool); ok && !ap {
					return fmt.Errorf("%s: unexpected property %q", path, k)
				}
				continue
			}
			if err := validate(sub, val, path+"."+k); err != nil {
				return err
			}
		}
	case []any:
		if err := bound(s, "minItems", "maxItems", float64(len(x)), path+" length"); err != nil {
			return err
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, el := range x {
				if err := validate(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case string:
		return bound(s, "minLength", "maxLength", float64(len([]rune(x))), path+" length")
	case float64:
		return bound(s, "minimum", "maximum", x, path)
	}
	return nil
}

func bound(s map[string]any, lo, hi string, n float64, what string) error {
	if m, ok := s[lo].(float64); ok && n < m {
		return fmt.Errorf("%s: %v below %s %v", what, n, lo, m)
	}
	if m, ok := s[hi].(float64); ok && n > m {
		return fmt.Errorf("%s: %v above %s %v", what, n, hi, m)
	}
	return nil
}

func typeMatches(t any, v any) bool {
	switch tt := t.(type) {
	case string:
		switch tt {
		case "object":
			_, ok := v.(map[string]any)
			return ok
		case "array":
			_, ok := v.([]any)
			return ok
		case "string":
			_, ok := v.(string)
			return ok
		case "boolean":
			_, ok := v.(bool)
			return ok
		case "null":
			return v == nil
		case "number":
			_, ok := v.(float64)
			return ok
		case "integer":
			f, ok := v.(float64)
			return ok && f == math.Trunc(f)
		}
		return false
	case []any:
		for _, x := range tt {
			if typeMatches(x, v) {
				return true
			}
		}
	}
	return false
}

// equal 比较 schema 里的值与实例里的值；数字在 validate 入口已转为 float64。
func equal(schemaVal, inst any) bool { return reflect.DeepEqual(schemaVal, inst) }

var (
	schemaKeywords = map[string]bool{"type": true, "enum": true, "const": true, "properties": true, "required": true,
		"additionalProperties": true, "items": true, "minimum": true, "maximum": true, "minLength": true,
		"maxLength": true, "minItems": true, "maxItems": true, "title": true, "description": true}
	schemaTypes = map[string]bool{"object": true, "array": true, "string": true, "boolean": true, "null": true, "number": true, "integer": true}
)

// checkSchema 拒绝子集之外的关键字与形态，避免不受支持的约束被静默忽略而放过不合规输出。
func checkSchema(s map[string]any, path string) error {
	for k, v := range s {
		if !schemaKeywords[k] {
			return fmt.Errorf("%s: unsupported keyword %q", path, k)
		}
		switch k {
		case "type":
			ts, _ := v.([]any)
			if t, ok := v.(string); ok {
				ts = []any{t}
			}
			if len(ts) == 0 {
				return fmt.Errorf("%s: bad type", path)
			}
			for _, t := range ts {
				if name, _ := t.(string); !schemaTypes[name] {
					return fmt.Errorf("%s: unknown type %v", path, t)
				}
			}
		case "enum", "required":
			if _, ok := v.([]any); !ok {
				return fmt.Errorf("%s: %s must be an array", path, k)
			}
		case "additionalProperties":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s: additionalProperties must be a boolean", path)
			}
		case "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems":
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("%s: %s must be a number", path, k)
			}
		case "items":
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: items must be a schema object", path)
			}
			if err := checkSchema(sub, path+".items"); err != nil {
				return err
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: properties must be an object", path)
			}
			for name, p := range props {
				sub, ok := p.(map[string]any)
				if !ok {
					return fmt.Errorf("%s.%s: property schema must be an object", path, name)
				}
				if err := checkSchema(sub, path+"."+name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
