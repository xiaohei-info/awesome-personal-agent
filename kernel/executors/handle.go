package executors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// 句柄与委派记录字段的校验（§6.17）：内核只看结构，不解释、不执行。这是基本拦截，不是脱敏保证；
// 拒绝时 why 只写键路径与规则名，不回显取值（§14.10）。

const (
	handleMaxBytes = 2048
	handleMaxDepth = 3
	handleMaxKeys  = 32
	handleMaxStr   = 256
)

var (
	handleKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// 键名按 "_" 切分后的任一段命中即拒绝：句柄不承载命令、提示词与凭据。
	forbiddenSegments = map[string]string{
		"command": "command", "cmd": "command", "argv": "command", "script": "command", "shell": "command", "exec": "command",
		"prompt": "prompt", "instruction": "prompt", "instructions": "prompt", "goal": "prompt", "body": "prompt",
		"content": "prompt", "message": "prompt", "env": "command", "headers": "credential",
		"token": "credential", "secret": "credential", "password": "credential", "passwd": "credential",
		"credential": "credential", "credentials": "credential", "auth": "credential", "authorization": "credential",
		"cookie": "credential", "apikey": "credential",
	}
	forbiddenPairs = [][2]string{{"api", "key"}, {"private", "key"}, {"access", "key"}}

	jwtShape     = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.`)
	urlUserinfo  = regexp.MustCompile(`://[^/\s@]*:[^/\s@]*@`)
	urlSecretArg = regexp.MustCompile(`(?i)[?&](token|access_token|key|sig|signature)=`)
	// APA 自己的凭据前缀（auth、runidentity）。
	apaCredential = regexp.MustCompile(`apa_(obs|wrk|dlg|ext|whk|run)_`)
)

func badField(path, rule string) error {
	return invalid("%s rejected by rule %s", path, rule)
}

// CheckValue 对一个字符串字段做单行、限长与凭据形态检查；path 用于报错。
func CheckValue(path, v string, max int) error {
	if len([]rune(v)) > max {
		return badField(path, fmt.Sprintf("max_length_%d", max))
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return badField(path, "single_line")
		}
	}
	low := strings.ToLower(v)
	switch {
	case strings.HasPrefix(low, "bearer "), strings.HasPrefix(low, "basic "), strings.Contains(v, "-----BEGIN"),
		jwtShape.MatchString(v), urlUserinfo.MatchString(v), urlSecretArg.MatchString(v), apaCredential.MatchString(v):
		return badField(path, "credential_shape")
	}
	return nil
}

func checkKey(path, k string) error {
	if !handleKey.MatchString(k) {
		return badField(path, "key_format")
	}
	segs := strings.Split(k, "_")
	for _, s := range segs {
		if rule, ok := forbiddenSegments[s]; ok {
			return badField(path, "no_"+rule)
		}
	}
	for i := 1; i < len(segs); i++ {
		for _, p := range forbiddenPairs {
			if segs[i-1] == p[0] && segs[i] == p[1] {
				return badField(path, "no_credential")
			}
		}
	}
	return nil
}

// ValidateHandle 校验不透明句柄：JSON 对象、≤ 2 KiB、嵌套 ≤ 3 层、键 ≤ 32 个，键名与取值按上面的规则。
// 空（缺省或 null）合法。返回规范化后的紧凑 JSON。
func ValidateHandle(path string, raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, badField(path, "json")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, badField(path, "object")
	}
	keys := 0
	var walk func(p string, v any, depth int) error
	walk = func(p string, v any, depth int) error {
		if depth > handleMaxDepth {
			return badField(p, "max_depth_3")
		}
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				keys++
				if keys > handleMaxKeys {
					return badField(path, "max_keys_32")
				}
				if err := checkKey(p+"."+k, k); err != nil {
					return err
				}
				if err := walk(p+"."+k, c, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for i, c := range t {
				if err := walk(fmt.Sprintf("%s[%d]", p, i), c, depth+1); err != nil {
					return err
				}
			}
		case string:
			return CheckValue(p, t, handleMaxStr)
		}
		return nil
	}
	if err := walk(path, obj, 1); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, badField(path, "json")
	}
	if out.Len() > handleMaxBytes {
		return nil, badField(path, "max_size_2KiB")
	}
	return out.Bytes(), nil
}
