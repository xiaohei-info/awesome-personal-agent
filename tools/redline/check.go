package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	modulePath = "github.com/xiaohei-info/awesome-personal-agent"
	outcallDir = "kernel/outcall" // 唯一外呼出口
)

// 内核（外呼调用器除外）禁止的网络与子进程库；net/url 等纯解析包不发起外呼，放行。
var (
	bannedExact    = map[string]bool{"net": true, "syscall": true, "os/exec": true}
	bannedPrefixes = []string{"net/", "golang.org/x/net", "google.golang.org/grpc", "nhooyr.io/websocket"}
	allowedNet     = map[string]bool{"net/url": true, "net/netip": true}
)

// 具体产品名（小写单词）。harness 名同时约束 sources/ext/packs。
var (
	harnessNames = []string{"hermes", "claude", "codex", "dsh"}
	productNames = append([]string{"telegram", "openai", "anthropic", "gemini", "himalaya", "gog",
		"slack", "discord", "feishu", "dingtalk", "wechat", "github"}, harnessNames...)
)

// Violation 是一条红线违规。
type Violation struct {
	Rule string
	File string // 相对仓库根，斜杠分隔
	Line int
	Msg  string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: [%s] %s", v.File, v.Line, v.Rule, v.Msg)
}

// Check 扫描 root 下的 kernel/（Go 非测试文件）与 sources/ext/packs/，缺失的目录视为无代码。
func Check(root string) ([]Violation, error) {
	var vs []Violation
	err := walk(root, "kernel", func(rel string, data []byte) error {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		got, err := checkKernelFile(rel, data)
		vs = append(vs, got...)
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"sources", "ext", "packs"} {
		err := walk(root, dir, func(rel string, data []byte) error {
			if bytes.IndexByte(data, 0) >= 0 { // 二进制文件
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if n := firstName(line, harnessNames); n != "" {
					vs = append(vs, Violation{"harness-name", rel, i + 1, "harness 名 " + n + " 只允许出现在 plugins/<harness>"})
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].File != vs[j].File {
			return vs[i].File < vs[j].File
		}
		return vs[i].Line < vs[j].Line
	})
	return vs, nil
}

func walk(root, dir string, fn func(rel string, data []byte) error) error {
	base := filepath.Join(root, dir)
	if _, err := os.Stat(root); err != nil {
		return err
	}
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		return fn(filepath.ToSlash(rel), data)
	})
}

func checkKernelFile(rel string, data []byte) ([]Violation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, data, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	inOutcall := rel == outcallDir || strings.HasPrefix(rel, outcallDir+"/")
	var vs []Violation
	add := func(rule string, pos token.Pos, msg string) {
		vs = append(vs, Violation{rule, rel, fset.Position(pos).Line, msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ImportSpec:
			p, _ := strconv.Unquote(x.Path.Value)
			if !inOutcall && bannedImport(p) {
				add("kernel-import", x.Pos(), "内核只能经 "+outcallDir+" 外呼，禁止 import "+p)
			}
			if name := firstName(importWords(p), productNames); name != "" {
				add("kernel-name", x.Pos(), "内核 import 出现产品名 "+name+": "+p)
			}
		case *ast.Ident:
			if name := firstName(x.Name, productNames); name != "" {
				add("kernel-name", x.Pos(), "内核标识符出现产品名 "+name+": "+x.Name)
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if name := firstName(strings.ReplaceAll(x.Value, modulePath, ""), productNames); name != "" {
					add("kernel-name", x.Pos(), "内核字符串出现产品名 "+name)
				}
			}
		}
		return true
	})
	return vs, nil
}

func bannedImport(p string) bool {
	if allowedNet[p] || strings.HasPrefix(p, modulePath) {
		return false
	}
	if bannedExact[p] {
		return true
	}
	for _, pre := range bannedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// importWords 去掉自身模块前缀和域名段，避免 github.com 之类被当成产品名。
func importWords(p string) string {
	if p == modulePath || strings.HasPrefix(p, modulePath+"/") {
		return ""
	}
	if i := strings.Index(p, "/"); i > 0 && strings.Contains(p[:i], ".") {
		p = p[i:]
	}
	return p
}

// firstName 把文本拆成小写单词（驼峰、下划线、连字符、点、斜杠均为分隔），返回第一个命中的名字。
func firstName(s string, names []string) string {
	s = strings.ReplaceAll(s, modulePath, "")
	for _, w := range words(s) {
		for _, n := range names {
			if w == n {
				return n
			}
		}
	}
	return ""
}

func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && len(cur) > 0 &&
			(unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]))):
			flush() // fooBar、HTTPServer → foo|bar、http|server
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}
