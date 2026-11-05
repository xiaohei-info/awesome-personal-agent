package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是导出配置。Idle 是无事件时两次轮询的间隔（内核长轮询之外的保险），Backoff 是可重试错误后的等待。
type Config struct {
	URL, Key, Dir string
	Wait          int // 长轮询秒数
	Idle, Backoff time.Duration
}

// ConfigFromEnv 读取环境变量；缺任何一项都是配置错误。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{URL: getenv("APA_URL"), Key: getenv("APA_EXT_KEY"), Dir: getenv("APA_EXPORT_DIR"), Wait: 50, Idle: time.Second, Backoff: 5 * time.Second}
	for name, v := range map[string]string{"APA_URL": c.URL, "APA_EXT_KEY": c.Key, "APA_EXPORT_DIR": c.Dir} {
		if v == "" {
			return Config{}, errors.New(name + " is required")
		}
	}
	return c, nil
}

const cursorFile = ".apa-export-cursor"

var safeID = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// statusError 是内核的非 2xx 回应。
type statusError struct {
	path string
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.path, e.code, e.body)
}

type envelope struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
}

type exporter struct {
	Config
	http *http.Client
}

// Run 订阅并导出，直到 ctx 取消（返回 nil）或遇到不可恢复的错误。网络错误与 5xx 退避重试；
// 凭据被拒（401/403）等其他 4xx 与不安全的对象 id 直接返回，不静默吞掉。
func Run(ctx context.Context, cfg Config) error {
	x := &exporter{Config: cfg, http: &http.Client{Timeout: time.Duration(cfg.Wait+30) * time.Second}}
	cursor, err := readCursor(cfg.Dir)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		next, n, err := x.step(ctx, cursor)
		switch {
		case err == nil:
			cursor = next
			if n == 0 {
				sleep(ctx, cfg.Idle)
			}
		case ctx.Err() != nil:
		case retryable(err):
			fmt.Fprintln(os.Stderr, "export: retrying:", err)
			sleep(ctx, cfg.Backoff)
		default:
			return err
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	var ue *url.Error
	return errors.As(err, &ue) // 连接失败、超时
}

// step 读一页事件、导出其中的对象、再落盘游标；返回新游标和本页信封数。
func (x *exporter) step(ctx context.Context, cursor int64) (int64, int, error) {
	var page struct {
		Events []envelope `json:"events"`
		Next   int64      `json:"next"`
	}
	q := fmt.Sprintf("/events?cursor=%d&wait=%d", cursor, x.Wait)
	if err := x.get(ctx, q, &page); err != nil {
		return cursor, 0, err
	}
	done := map[string]bool{}
	for _, e := range page.Events {
		base := ""
		switch {
		case strings.HasPrefix(e.Type, "item."):
			base = "items"
		case strings.HasPrefix(e.Type, "material."):
			base = "materials"
		default:
			continue
		}
		if !safeID.MatchString(e.Object) {
			return cursor, 0, fmt.Errorf("event %d has an unsafe object id %q", e.Seq, e.Object)
		}
		if done[base+"/"+e.Object] {
			continue
		}
		done[base+"/"+e.Object] = true
		if err := x.export(ctx, base, e.Object); err != nil {
			return cursor, 0, err
		}
	}
	if page.Next > cursor {
		if err := writeFileAtomic(filepath.Join(x.Dir, cursorFile), []byte(strconv.FormatInt(page.Next, 10)+"\n")); err != nil {
			return cursor, 0, err
		}
		cursor = page.Next
	}
	return cursor, len(page.Events), nil
}

func (x *exporter) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(x.URL, "/")+"/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+x.Key)
	req.Header.Set("Accept", "application/json")
	resp, err := x.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &statusError{path: path, code: resp.StatusCode, body: strings.TrimSpace(string(data[:min(len(data), 200)]))}
	}
	return json.Unmarshal(data, out)
}

// 只导出这些字段：账本里的 ask、spec、ext 等内部结构不外流。
var (
	itemFields     = []string{"id", "kind", "phase", "title", "summary", "status_label", "priority", "project", "tags", "assignee", "deadline", "version"}
	materialFields = []string{"id", "kind", "title", "project", "tags", "location", "current_rev"}
)

// export 取一个对象并覆盖写它的 Markdown。对象已不存在（404）则跳过，不让游标卡在它身上。
func (x *exporter) export(ctx context.Context, base, id string) error {
	var doc map[string]any
	if err := x.get(ctx, "/"+base+"/"+id, &doc); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return nil
		}
		return err
	}
	var meta map[string]any
	var body string
	if base == "items" {
		meta, _ = doc["item"].(map[string]any)
		body, _ = meta["body"].(string)
	} else {
		meta, _ = doc["material"].(map[string]any)
		// 托管型材料只回位置（有 location），正文不由内核提供；其余取当前修订的全文。
		if rev, ok := doc["revision"].(map[string]any); ok && meta["location"] == nil {
			body, _ = rev["content"].(string)
		}
	}
	if meta == nil {
		return fmt.Errorf("GET /%s/%s: unexpected response shape", base, id)
	}
	fields := itemFields
	if base == "materials" {
		fields = materialFields
	}
	front := yaml.Node{Kind: yaml.MappingNode}
	for _, f := range fields {
		v, ok := meta[f]
		if !ok || v == nil {
			continue
		}
		var val yaml.Node
		if err := val.Encode(v); err != nil {
			return err
		}
		front.Content = append(front.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: f}, &val)
	}
	fm, err := yaml.Marshal(&front)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(fm)
	buf.WriteString("---\n")
	if body != "" {
		buf.WriteString("\n" + strings.TrimRight(body, "\n") + "\n")
	}
	return writeFileAtomic(filepath.Join(x.Dir, base, id+".md"), buf.Bytes())
}

func readCursor(dir string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, cursorFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("cursor file %s is corrupt", filepath.Join(dir, cursorFile))
	}
	return n, nil
}

// writeFileAtomic 先写临时文件再改名，笔记软件不会读到半个文件。
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
