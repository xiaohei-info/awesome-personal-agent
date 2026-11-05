// Package dirwatch 是本地目录监视拉取程序（设计 §9.1、§9.2）：
// 一次性运行（非常驻）：stdin 读 Source 契约输入，把上次游标之后新建和修改的文件转成标准事件 JSONL，末行是新游标。
// 游标是 (mtime, 路径) 水位线，不写账本、不访问内核存储。
package dirwatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 退出码沿用 §9.2 的 Source 契约。
const (
	ExitOK        = 0
	ExitTemporary = 1 // 目录暂时读不到（权限、IO）
	ExitBadConfig = 2 // 配置不合法或目录不存在：生成 needs_input
)

// Input 是 stdin 的 Source 契约输入；config.dir 是要监视的目录（递归）。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Cursor   string `json:"cursor"`
	Config   struct {
		Dir string `json:"dir"`
	} `json:"config"`
}

type event struct {
	ID              string    `json:"id"`
	Type            string    `json:"type"`
	Subject         string    `json:"subject"`
	Summary         string    `json:"summary,omitempty"`
	Link            string    `json:"link,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
	CorrelationKeys []string  `json:"correlation_keys,omitempty"`
}

// cursor 是 O(1) 水位线：已输出到 (mtime, 路径) 为止，按这个顺序严格大于它的文件才是新的。
// shortcut: 无法区分新建与修改（统一 file.changed），也漏掉 mtime 早于水位线才出现的文件（如 cp -p/mv 进来），要区分时再升级。
type cursor struct {
	MTime int64  `json:"t"`
	Path  string `json:"p"`
}

func (c cursor) before(mtime int64, path string) bool {
	return mtime > c.MTime || (mtime == c.MTime && path > c.Path)
}

// outputBudget 是单次 stdout 的字节上限，低于 runner 保留的 64KiB（kernel/sources/runner/parse.go），
// 超出的文件留到下次运行，游标只推进到已输出的最后一个。
const outputBudget = 48 << 10

type runErr struct {
	exit int
	msg  string
}

func (e *runErr) Error() string { return e.msg }

// Run 执行一次拉取并返回退出码；事件写 stdout，诊断写 stderr。
func Run(stdin io.Reader, stdout, stderr io.Writer) int {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		fmt.Fprintf(stderr, "stdin 不是合法的 JSON: %v\n", err)
		return ExitBadConfig
	}
	out, err := poll(in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		var re *runErr
		if errors.As(err, &re) {
			return re.exit
		}
		return ExitTemporary
	}
	enc := json.NewEncoder(stdout)
	for _, e := range out.events {
		_ = enc.Encode(e)
	}
	_ = enc.Encode(map[string]string{"cursor": out.cursor})
	return ExitOK
}

type result struct {
	events []event
	cursor string
}

func poll(in Input) (*result, error) {
	dir := in.Config.Dir
	if strings.TrimSpace(dir) == "" || !filepath.IsAbs(dir) {
		return nil, &runErr{ExitBadConfig, "config.dir 必须是绝对路径"}
	}
	st, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, &runErr{ExitBadConfig, fmt.Sprintf("监视目录不存在: %s", dir)}
	case err != nil:
		return nil, &runErr{ExitTemporary, fmt.Sprintf("监视目录不可读: %v", err)}
	case !st.IsDir():
		return nil, &runErr{ExitBadConfig, fmt.Sprintf("config.dir 不是目录: %s", dir)}
	}
	var prev cursor
	if in.Cursor != "" {
		if err := json.Unmarshal([]byte(in.Cursor), &prev); err != nil {
			prev = cursor{} // 游标损坏：当作首次，去重按事件 id 兜底
		}
	}
	type file struct {
		rel   string
		path  string
		mtime time.Time
		size  int64
	}
	var files []file
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // 扫描中被删，下次再看
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if prev.before(info.ModTime().UnixNano(), rel) {
			files = append(files, file{rel, p, info.ModTime(), info.Size()})
		}
		return nil
	})
	if err != nil {
		return nil, &runErr{ExitTemporary, fmt.Sprintf("扫描目录失败: %v", err)}
	}
	sort.Slice(files, func(i, j int) bool {
		if a, b := files[i].mtime.UnixNano(), files[j].mtime.UnixNano(); a != b {
			return a < b
		}
		return files[i].rel < files[j].rel
	})
	var evs []event
	used, next := 0, prev
	for _, f := range files {
		e := event{
			ID:   "changed:" + f.rel + ":" + strconv.FormatInt(f.mtime.UnixNano(), 10) + ":" + strconv.FormatInt(f.size, 10),
			Type: "file.changed", Subject: f.rel, Summary: f.rel + " changed", Link: "file://" + filepath.ToSlash(f.path),
			OccurredAt: f.mtime.UTC(), CorrelationKeys: []string{"dir:" + dir + ":" + f.rel},
		}
		line, _ := json.Marshal(e)
		if len(evs) > 0 && used+len(line)+1 > outputBudget {
			break
		}
		used += len(line) + 1
		evs = append(evs, e)
		next = cursor{f.mtime.UnixNano(), f.rel}
	}
	b, _ := json.Marshal(next) // 固定结构
	return &result{events: evs, cursor: string(b)}, nil
}
