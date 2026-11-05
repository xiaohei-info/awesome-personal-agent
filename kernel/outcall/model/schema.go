package model

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
)

// validate 实现结构化输出用到的 JSON Schema 子集：type、enum、const、properties、required、
// additionalProperties(false)、items、minimum、maximum、minLength、maxLength、minItems、maxItems。
// shortcut: 不支持 $ref/oneOf/anyOf/pattern；能力包需要时再扩展，不引入第三方校验库。
func validate(s map[string]any, v any, path string) error {
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
	case json.Number:
		return bound(s, "minimum", "maximum", num(x), path)
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

func num(n json.Number) float64 {
	f, _ := new(big.Float).SetString(n.String())
	out, _ := f.Float64()
	return out
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
			_, ok := v.(json.Number)
			return ok
		case "integer":
			n, ok := v.(json.Number)
			if !ok {
				return false
			}
			f, _ := new(big.Float).SetString(n.String())
			return f != nil && f.IsInt()
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

// equal 比较 schema 里的值（float64）与实例里的值（json.Number）。
func equal(schemaVal, inst any) bool {
	if n, ok := inst.(json.Number); ok {
		f, ok := schemaVal.(float64)
		return ok && num(n) == f
	}
	return reflect.DeepEqual(schemaVal, inst)
}
