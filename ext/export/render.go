package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---- 路径 ----

const archiveDir = "_archive"

var placeholder = regexp.MustCompile(`\{(project|kind|id|title|planned_for|phase)\}`)

// cleanName 把一段取值清洗成安全的文件名成分：非法字符与控制字符换成 _，去掉首尾空白与点，限长。
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(strings.TrimSpace(s), ". ")
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	if s == "" {
		return "_"
	}
	return s
}

// expandPath 把路径模板展开成 root 内的相对路径。取值逐个清洗，模板自己的 / 是目录分隔；
// 含 .. 或绝对路径的模板直接报错，写出目录之外不可能发生。
func expandPath(tpl string, vals map[string]string) (string, error) {
	out := placeholder.ReplaceAllStringFunc(tpl, func(m string) string { return cleanName(vals[m[1:len(m)-1]]) })
	if path.IsAbs(out) || strings.HasPrefix(out, ".apa-export") || slices.Contains(strings.Split(out, "/"), "..") {
		return "", fmt.Errorf("path template %q expands outside the export root", tpl)
	}
	out = path.Clean(out)
	if out == "." {
		return "", fmt.Errorf("path template %q expands to nothing", tpl)
	}
	return filepath.FromSlash(out), nil
}

// ---- select ----

type item struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Phase       string          `json:"phase"`
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	StatusLabel string          `json:"status_label"`
	Priority    string          `json:"priority"`
	Project     string          `json:"project"`
	Tags        []string        `json:"tags"`
	Assignee    string          `json:"assignee"`
	Deadline    *time.Time      `json:"deadline"`
	PlannedFor  string          `json:"planned_for"`
	Body        string          `json:"body"`
	Spec        json.RawMessage `json:"spec"`
	Version     int64           `json:"version"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Ask         *struct {
		ScreeningPending bool `json:"screening_pending"`
	} `json:"ask"`
}

type relation struct{ Type, From, To string }
type progress struct {
	Text string    `json:"text"`
	On   string    `json:"on"`
	At   time.Time `json:"at"`
}

// screening 报告 web_only 初筛是否未返回：内核此时只给 id、阶段、版本，没有内容可导出。
func (it item) screening() bool { return it.Ask != nil && it.Ask.ScreeningPending }

func (s Select) matches(it item, now time.Time) bool {
	in := func(list []string, v string) bool { return len(list) == 0 || slices.Contains(list, v) }
	if !in(s.Projects, it.Project) || !in(s.Kinds, it.Kind) || !in(s.Phases, it.Phase) {
		return false
	}
	if len(s.Tags) > 0 && !slices.ContainsFunc(it.Tags, func(t string) bool { return slices.Contains(s.Tags, t) }) {
		return false
	}
	if len(s.Phases) == 0 && (it.Phase == "done" || it.Phase == "dropped") && !it.UpdatedAt.IsZero() {
		return now.Sub(it.UpdatedAt) <= time.Duration(s.RecentDays)*24*time.Hour
	}
	return true
}

// ---- 渲染 ----

func (o Options) itemPath(it item) (string, error) {
	return expandPath(o.Path, map[string]string{"project": it.Project, "kind": it.Kind, "id": it.ID,
		"title": it.Title, "planned_for": it.PlannedFor, "phase": it.Phase})
}

func (o Options) webURL(id string) string {
	if o.PublicURL == "" {
		return ""
	}
	return o.PublicURL + "/items/" + url.PathEscape(id)
}

// fieldValue 取事项字段的导出值；空值返回 nil（未设置的字段不写）。
func (o Options) fieldValue(it item, f string) any {
	str := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	switch f {
	case "id":
		return str(it.ID)
	case "kind":
		return str(it.Kind)
	case "title":
		return str(it.Title)
	case "summary":
		return str(it.Summary)
	case "status_label":
		return str(it.StatusLabel)
	case "priority":
		return str(it.Priority)
	case "phase":
		return str(it.Phase)
	case "project":
		return str(it.Project)
	case "assignee":
		return str(it.Assignee)
	case "planned_for":
		return str(o.fmtDay(it.PlannedFor))
	case "url":
		return str(o.webURL(it.ID))
	case "version":
		return it.Version
	case "tags":
		if len(it.Tags) == 0 {
			return nil
		}
		return it.Tags
	case "deadline":
		if it.Deadline == nil {
			return nil
		}
		return o.fmtTime(*it.Deadline)
	case "updated_at":
		if it.UpdatedAt.IsZero() {
			return nil
		}
		return o.fmtTime(it.UpdatedAt)
	}
	return nil
}

func (o Options) frontMatter(it item) (string, error) {
	n := yaml.Node{Kind: yaml.MappingNode}
	for _, e := range o.FrontMatter {
		v := o.fieldValue(it, e.Field)
		if v == nil {
			continue
		}
		var val yaml.Node
		if err := val.Encode(v); err != nil {
			return "", err
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: e.Key}, &val)
	}
	if len(n.Content) == 0 {
		return "", nil
	}
	b, err := yaml.Marshal(&n)
	return "---\n" + string(b) + "---\n", err
}

// relLink 是从 from 所在目录到 root 下 to 的 Markdown 链接目标；路径含空格时用尖括号包起来。
func relLink(from, to string) string {
	r, err := filepath.Rel(filepath.Dir(from), to)
	if err != nil {
		return filepath.ToSlash(to)
	}
	return "<" + filepath.ToSlash(r) + ">"
}

// renderItem 渲染一个事项文件（不含生成标记）：头部、再按 body 配置的节顺序写正文。
// ask、spec 之外的内部结构、挂靠消息一概不取，所以不会外流。selfPath 用来算相对链接，childPath 查已导出的子事项路径。
func (o Options) renderItem(it item, rels []relation, prog []progress, selfPath string, childPath func(id string) string) (head, rest string, err error) {
	head, err = o.frontMatter(it)
	if err != nil {
		return "", "", err
	}
	var secs []string
	for _, s := range o.Body {
		switch s {
		case "summary":
			if it.Summary != "" {
				secs = append(secs, "## 摘要\n\n"+it.Summary)
			}
		case "body":
			var b []string
			if t := strings.TrimRight(it.Body, "\n"); t != "" {
				b = append(b, t)
			}
			if it.Kind == "brief" {
				if t := renderBrief(it.Spec); t != "" {
					b = append(b, t)
				}
			}
			if len(b) > 0 {
				secs = append(secs, strings.Join(b, "\n\n"))
			}
		case "progress":
			var l []string
			if it.PlannedFor != "" {
				l = append(l, "计划日期："+o.fmtDay(it.PlannedFor)+"\n")
			}
			for _, p := range prog {
				day := p.On
				if day == "" {
					day = p.At.Format("2006-01-02")
				}
				l = append(l, fmt.Sprintf("- %s %s", o.fmtDay(day), strings.ReplaceAll(strings.TrimSpace(p.Text), "\n", "\n  ")))
			}
			if len(l) > 0 {
				secs = append(secs, "## 进展\n\n"+strings.Join(l, "\n"))
			}
		case "subitems":
			var l []string
			for _, r := range rels {
				if r.Type == "parent" && r.To == it.ID {
					if p := childPath(r.From); p != "" {
						l = append(l, fmt.Sprintf("- [%s](%s)", r.From, relLink(selfPath, p)))
					} else {
						l = append(l, "- "+r.From)
					}
				}
			}
			if len(l) > 0 {
				secs = append(secs, "## 子事项\n\n"+strings.Join(l, "\n"))
			}
		case "materials":
			var l []string
			for _, r := range rels {
				if r.From == it.ID && (r.Type == "basis" || r.Type == "output" || r.Type == "minutes") {
					l = append(l, fmt.Sprintf("- %s：[%s](%s)", r.Type, r.To, relLink(selfPath, materialPath(r.To))))
				}
			}
			if len(l) > 0 {
				secs = append(secs, "## 关联材料\n\n"+strings.Join(l, "\n"))
			}
		}
	}
	if o.ActionLinks && o.PublicURL != "" { // 没有 public_url 就不写：不能写出指向本机监听地址的链接
		base := o.PublicURL + "/act/" + url.PathEscape(it.ID)
		secs = append(secs, fmt.Sprintf("## 动作\n\n- [完成](%s?do=done)\n- [写进展](%s?do=progress)\n- [改计划日期](%s?do=plan)", base, base, base))
	}
	if len(secs) > 0 {
		rest = "\n" + strings.Join(secs, "\n\n") + "\n"
	}
	return head, rest, nil
}

func materialPath(id string) string { return filepath.Join("materials", id+".md") }

func renderBrief(spec json.RawMessage) string {
	var s struct {
		Period   string `json:"period"`
		Sections []struct {
			Project string `json:"project"`
			Body    string `json:"body"`
		} `json:"sections"`
	}
	if json.Unmarshal(spec, &s) != nil {
		return ""
	}
	var out []string
	if s.Period != "" {
		out = append(out, "周期："+s.Period)
	}
	for _, sec := range s.Sections {
		out = append(out, "## "+sec.Project+"\n\n"+strings.TrimRight(sec.Body, "\n"))
	}
	return strings.Join(out, "\n\n")
}

// ---- 生成标记与手改检测 ----
//
// 文件 = 头部（front matter）+ 一行生成标记 + 其余内容。标记形如
//   <!-- apa-export id=<对象 id> version=<事项版本> hash=<sha256> -->
// hash 是「去掉标记行后的整个文件内容」的 sha256（十六进制）。头部放在标记之前，
// 笔记软件照常解析 front matter；标记之后手改任何一处，hash 都对不上。

var markerRE = regexp.MustCompile(`(?m)^<!-- apa-export id=(\S+) version=(\S*) hash=([0-9a-f]{64}) -->\n`)

func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func withMarker(id, version, head, rest string) string {
	return head + fmt.Sprintf("<!-- apa-export id=%s version=%s hash=%s -->\n", id, version, contentHash(head+rest)) + rest
}

// hasLocalEdit 报告文件内容是否不是 ext 上次写下的样子：没有标记、标记 id 不符或 hash 不符都算手改。
func hasLocalEdit(id, content string) bool {
	loc := markerRE.FindStringSubmatchIndex(content)
	if loc == nil {
		return true
	}
	m := markerRE.FindStringSubmatch(content)
	return m[1] != id || contentHash(content[:loc[0]]+content[loc[1]:]) != m[3]
}
