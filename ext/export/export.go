package main

import (
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
// Opts 是目录导出的版式配置（§10.3），来自 APA_EXPORT_CONFIG。
type Config struct {
	URL, Key, Dir string
	Wait          int // 长轮询秒数
	Idle, Backoff time.Duration
	Reconcile     time.Duration // 运行中重新对账的间隔：物理删除没有事件，只能靠对账发现
	Opts          Options
}

// ConfigFromEnv 读取环境变量；缺任何一项都是配置错误。目录取 APA_EXPORT_DIR，缺省取配置文件里的 root；
// public_url 取配置文件，缺省取 APA_PUBLIC_URL。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	o, err := loadOptions(getenv("APA_EXPORT_CONFIG"))
	if err != nil {
		return Config{}, err
	}
	c := Config{URL: getenv("APA_URL"), Key: getenv("APA_EXT_KEY"), Dir: getenv("APA_EXPORT_DIR"), Wait: 50, Idle: time.Second, Backoff: 5 * time.Second, Reconcile: time.Minute, Opts: o}
	if c.Dir == "" {
		c.Dir = o.Root
	}
	if c.Opts.PublicURL == "" {
		c.Opts.PublicURL = strings.TrimRight(getenv("APA_PUBLIC_URL"), "/")
	}
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
	http  *http.Client
	state *state
	sig   string // 版式配置的签名：配置变了，已导出的文件要按新版式重写
}

// Run 订阅并导出，直到 ctx 取消（返回 nil）或遇到不可恢复的错误。启动时先按事项列表对账一次
// （配置、select 变了，或停机期间有事项被删除，都靠它补上）；之后按事件增量导出。
// 网络错误与 5xx 退避重试；凭据被拒（401/403）等其他 4xx 与不安全的对象 id 直接返回，不静默吞掉。
func Run(ctx context.Context, cfg Config) error {
	st, err := loadState(cfg.Dir)
	if err != nil {
		return err
	}
	sig, _ := json.Marshal(cfg.Opts)
	x := &exporter{Config: cfg, http: &http.Client{Timeout: time.Duration(cfg.Wait+30) * time.Second}, state: st, sig: contentHash(string(sig))}
	cursor, err := readCursor(cfg.Dir)
	if err != nil {
		return err
	}
	synced, lastSync := false, time.Now()
	for ctx.Err() == nil {
		var n int
		if !synced || (cfg.Reconcile > 0 && time.Since(lastSync) >= cfg.Reconcile) {
			err = x.sync(ctx)
			synced = err == nil
			if synced {
				lastSync = time.Now()
			}
		} else {
			cursor, n, err = x.stepFrom(ctx, cursor)
		}
		switch {
		case err == nil:
			if n == 0 && synced {
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

// stepFrom 读一页事件、导出其中的对象、再落盘游标；返回新游标和本页信封数。出错时游标不前进。
func (x *exporter) stepFrom(ctx context.Context, cursor int64) (int64, int, error) {
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
		var bases []string
		switch {
		case strings.HasPrefix(e.Type, "item."), strings.HasPrefix(e.Type, "decision."), strings.HasPrefix(e.Type, "delegation."):
			bases = []string{"items"} // 决策与委派事件的对象是事项 id，不一定伴随 item.*
		case strings.HasPrefix(e.Type, "material."):
			bases = []string{"materials"}
		default:
			continue
		}
		if !safeID.MatchString(e.Object) {
			return cursor, 0, fmt.Errorf("event %d has an unsafe object id %q", e.Seq, e.Object)
		}
		for _, base := range bases {
			key := base + "/" + e.Object
			if done[key] {
				continue
			}
			done[key] = true
			var err error
			if base == "items" {
				err = x.exportItem(ctx, e.Object)
			} else {
				err = x.exportMaterial(ctx, e.Object)
			}
			if err != nil {
				return cursor, 0, err
			}
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

// sync 按 GET /items 与 GET /materials 对账：版本、版式、归档状态都没变的跳过，其余重写；列表里没有的已导出事项再确认一次（读不到才删）。
func (x *exporter) sync(ctx context.Context) error {
	var list struct {
		Items []item `json:"items"`
	}
	if err := x.get(ctx, "/items", &list); err != nil {
		return err
	}
	seen := map[string]bool{}
	now := time.Now()
	for i := range list.Items {
		it := list.Items[i]
		if !safeID.MatchString(it.ID) {
			return fmt.Errorf("GET /items: unsafe item id %q", it.ID)
		}
		seen[it.ID] = true
		if it.screening() {
			continue
		}
		if e := x.state.Files["items/"+it.ID]; e != nil && e.Version == it.Version && e.Sig == x.sig && e.Archived == !x.Opts.Select.matches(it, now) {
			continue
		}
		if err := x.exportItem(ctx, it.ID); err != nil {
			return err
		}
	}
	var mats struct {
		Materials []struct {
			ID string `json:"id"`
		} `json:"materials"`
	}
	if err := x.get(ctx, "/materials", &mats); err != nil {
		return err
	}
	seenMat := map[string]bool{}
	for _, m := range mats.Materials {
		seenMat[m.ID] = true
	}
	// 列表里没有的已导出对象再确认一次：读不到（404）才删。
	for key := range x.state.Files {
		var err error
		if id, ok := strings.CutPrefix(key, "items/"); ok && !seen[id] {
			err = x.exportItem(ctx, id)
		} else if id, ok := strings.CutPrefix(key, "materials/"); ok && !seenMat[id] {
			err = x.exportMaterial(ctx, id)
		}
		if err != nil {
			return err
		}
	}
	return nil
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

// exportItem 取一个事项并写它的 Markdown。读不到（404）说明已被物理删除，删除对应文件；
// web_only 初筛未返回的事项没有内容可导出，跳过，初筛返回后的更新事件会再触发。
func (x *exporter) exportItem(ctx context.Context, id string) error {
	key := "items/" + id
	var doc struct {
		Item      item       `json:"item"`
		Relations []relation `json:"relations"`
		Progress  []progress `json:"progress"`
	}
	if err := x.get(ctx, "/items/"+id, &doc); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return x.drop(key)
		}
		return err
	}
	it := doc.Item
	if it.ID == "" {
		return fmt.Errorf("GET /items/%s: unexpected response shape", id)
	}
	if it.screening() {
		return nil
	}
	o := x.Opts
	if it.Kind == "brief" && !o.briefs() {
		return x.drop(key)
	}
	archived := !o.Select.matches(it, time.Now())
	rel, err := o.itemPath(it)
	if err != nil {
		return err
	}
	if archived {
		rel = filepath.Join(archiveDir, rel)
	}
	rel = x.uniquePath(key, id, rel)
	head, rest, err := o.renderItem(it, doc.Relations, doc.Progress, rel, func(c string) string {
		if e := x.state.Files["items/"+c]; e != nil {
			return e.Path
		}
		return ""
	})
	if err != nil {
		return err
	}
	return x.write(key, id, strconv.FormatInt(it.Version, 10), it.Version, rel, head, rest, archived)
}

// 材料只导出这些字段：托管型只回位置，其余取当前修订的全文。
var materialFields = []string{"id", "kind", "title", "project", "tags", "location", "current_rev"}

func (x *exporter) exportMaterial(ctx context.Context, id string) error {
	key := "materials/" + id
	var doc map[string]any
	if err := x.get(ctx, "/materials/"+id, &doc); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return x.drop(key)
		}
		return err
	}
	meta, _ := doc["material"].(map[string]any)
	if meta == nil {
		return fmt.Errorf("GET /materials/%s: unexpected response shape", id)
	}
	var body string
	if rev, ok := doc["revision"].(map[string]any); ok && meta["location"] == nil {
		body, _ = rev["content"].(string)
	}
	front := yaml.Node{Kind: yaml.MappingNode}
	for _, f := range materialFields {
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
	rest := ""
	if body != "" {
		rest = "\n" + strings.TrimRight(body, "\n") + "\n"
	}
	rev, _ := meta["current_rev"].(string)
	return x.write(key, id, rev, 0, materialPath(id), "---\n"+string(fm)+"---\n", rest, false)
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
