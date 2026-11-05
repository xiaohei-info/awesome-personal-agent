// Package dirwatch 是本地目录监视拉取程序（设计 §9.1、§9.2）：
// 一次性运行（非常驻）：stdin 读 Source 契约输入，把上次游标之后新建和修改的文件转成标准事件 JSONL，末行是新游标。
// 游标是目录快照（相对路径 → 修改时间与大小），不写账本、不访问内核存储。
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

// snapshot：相对路径（斜杠分隔）→ "修改时间纳秒:大小"。
// shortcut: 游标随文件数线性增长，目录文件数上万时改成按 mtime 水位线，再升级。
type snapshot map[string]string

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
	var prev snapshot
	if in.Cursor != "" {
		if err := json.Unmarshal([]byte(in.Cursor), &prev); err != nil {
			prev = nil // 游标损坏：当作首次，去重按事件 id 兜底
		}
	}
	cur := snapshot{}
	var evs []event
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
		sig := strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(info.Size(), 10)
		cur[rel] = sig
		old, seen := prev[rel]
		if seen && old == sig {
			return nil
		}
		typ, verb := "file.created", "created"
		if seen {
			typ, verb = "file.modified", "modified"
		}
		evs = append(evs, event{
			ID: verb + ":" + rel + ":" + sig, Type: typ, Subject: rel,
			Summary: fmt.Sprintf("%s %s", rel, verb), Link: "file://" + filepath.ToSlash(p),
			OccurredAt: info.ModTime().UTC(), CorrelationKeys: []string{"dir:" + dir + ":" + rel},
		})
		return nil
	})
	if err != nil {
		return nil, &runErr{ExitTemporary, fmt.Sprintf("扫描目录失败: %v", err)}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Subject < evs[j].Subject })
	b, _ := json.Marshal(cur) // map[string]string 不会失败
	return &result{events: evs, cursor: string(b)}, nil
}
