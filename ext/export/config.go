package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Options 是 §10.3「Markdown 目录导出」的配置表，来自 APA_EXPORT_CONFIG 指向的 YAML。
// 不给配置文件时取默认值，输出与 v0.1 的 items/<id>.md 版式一致。
type Options struct {
	Root        string   `yaml:"root"`
	PublicURL   string   `yaml:"public_url"`
	Select      Select   `yaml:"select"`
	Path        string   `yaml:"path"`
	FrontMatter fmMap    `yaml:"front_matter"`
	DateFormat  string   `yaml:"date_format"` // 如 YYYY-MM-DD；空则日期原样、时刻用 RFC 3339
	Body        []string `yaml:"body"`
	ActionLinks bool     `yaml:"action_links"`
	Briefs      *bool    `yaml:"briefs"` // 缺省导出
}

// Select 按项目、阶段、kind、标签筛选；各项之间取交集。没写 phases 时默认未终结的事项，加最近 RecentDays 天内终结的。
type Select struct {
	Projects   []string `yaml:"projects"`
	Phases     []string `yaml:"phases"`
	Kinds      []string `yaml:"kinds"`
	Tags       []string `yaml:"tags"`
	RecentDays int      `yaml:"recent_days"`
}

type fmEntry struct{ Key, Field string }

// fmMap 保持配置里键的顺序：文件头部按这个顺序写。
type fmMap []fmEntry

func (m *fmMap) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("front_matter must be a mapping of key to item field")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		*m = append(*m, fmEntry{n.Content[i].Value, n.Content[i+1].Value})
	}
	return nil
}

// fieldAlias 让配置既能写事项字段名，也能写设计文档里的中文名。
var fieldAlias = map[string]string{
	"标题": "title", "阶段": "phase", "项目": "project", "标签": "tags", "截止": "deadline",
	"计划日期": "planned_for", "执行者": "assignee", "更新时间": "updated_at", "Web 地址": "url",
}

var knownFields = map[string]bool{"id": true, "kind": true, "title": true, "summary": true, "status_label": true, "priority": true,
	"phase": true, "project": true, "tags": true, "assignee": true, "deadline": true, "planned_for": true,
	"updated_at": true, "version": true, "url": true}

var knownSections = map[string]bool{"summary": true, "body": true, "progress": true, "subitems": true, "materials": true}

const defaultPath = "items/{id}.md"

// defaultFront 是不配置时的头部：v0.1 导出的那组字段。
var defaultFront = fmMap{{"id", "id"}, {"kind", "kind"}, {"phase", "phase"}, {"title", "title"}, {"summary", "summary"},
	{"status_label", "status_label"}, {"priority", "priority"}, {"project", "project"}, {"tags", "tags"},
	{"assignee", "assignee"}, {"deadline", "deadline"}, {"version", "version"}}

func loadOptions(path string) (Options, error) {
	var o Options
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return o, fmt.Errorf("APA_EXPORT_CONFIG: %w", err)
		}
		d := yaml.NewDecoder(strings.NewReader(string(b)))
		d.KnownFields(true)
		if err := d.Decode(&o); err != nil && !errors.Is(err, io.EOF) {
			return o, fmt.Errorf("APA_EXPORT_CONFIG %s: %w", path, err)
		}
	}
	return o, o.normalize()
}

func (o *Options) normalize() error {
	if o.Path == "" {
		o.Path = defaultPath
	}
	if o.FrontMatter == nil {
		o.FrontMatter = defaultFront
	}
	for i, e := range o.FrontMatter {
		if a, ok := fieldAlias[e.Field]; ok {
			o.FrontMatter[i].Field = a
		}
		if !knownFields[o.FrontMatter[i].Field] {
			return fmt.Errorf("front_matter %q: unknown item field %q", e.Key, e.Field)
		}
	}
	if len(o.Body) == 0 {
		o.Body = []string{"body"}
	}
	for _, s := range o.Body {
		if !knownSections[s] {
			return fmt.Errorf("body: unknown section %q", s)
		}
	}
	if o.Select.RecentDays <= 0 {
		o.Select.RecentDays = 30
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	if _, err := expandPath(o.Path, map[string]string{}); err != nil {
		return err
	}
	return nil
}

func (o Options) briefs() bool { return o.Briefs == nil || *o.Briefs }

// timeLayout 把 YYYY-MM-DD HH:mm:ss 风格的日期格式转成 Go 布局。
func timeLayout(f string) string {
	return strings.NewReplacer("YYYY", "2006", "MM", "01", "DD", "02", "HH", "15", "mm", "04", "ss", "05").Replace(f)
}

func (o Options) fmtTime(t time.Time) string {
	if o.DateFormat == "" {
		return t.Format(time.RFC3339)
	}
	return t.Format(timeLayout(o.DateFormat))
}

// fmtDay 格式化 YYYY-MM-DD 的计划日期；无法解析的原样返回。
func (o Options) fmtDay(s string) string {
	if o.DateFormat == "" {
		return s
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return t.Format(timeLayout(o.DateFormat))
}
