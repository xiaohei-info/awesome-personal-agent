package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stateFile = ".apa-export-state.json"

// entry 记一个已导出对象：文件在 root 下的相对路径、事项版本、版式签名、是否已归档。
type entry struct {
	Path     string `json:"path"`
	Version  int64  `json:"version"`
	Sig      string `json:"sig"`
	Archived bool   `json:"archived,omitempty"`
}

// edit 是一条手改记录：手改过的文件被另存为 Copy 后才写入新内容。
type edit struct {
	Path string    `json:"path"`
	Copy string    `json:"copy"`
	At   time.Time `json:"at"`
}

// state 是导出状态，落在 root 下。账本是事实来源，状态只记「我写过什么、写在哪」，丢了也能靠全量对账重建。
type state struct {
	dir   string
	Files map[string]*entry `json:"files"`
	Edits []edit            `json:"edits,omitempty"`
}

func loadState(dir string) (*state, error) {
	s := &state{dir: dir, Files: map[string]*entry{}}
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil || s.Files == nil {
		return nil, fmt.Errorf("export state %s is corrupt", filepath.Join(dir, stateFile))
	}
	return s, nil
}

func (s *state) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, stateFile), b)
}

// uniquePath 在 rel 已被另一个对象占用时，在扩展名前追加事项 id。
func (x *exporter) uniquePath(key, id, rel string) string {
	for k, e := range x.state.Files {
		if k != key && e.Path == rel {
			ext := filepath.Ext(rel)
			return strings.TrimSuffix(rel, ext) + "-" + id + ext
		}
	}
	return rel
}

func (x *exporter) abs(rel string) string { return filepath.Join(x.Dir, rel) }

// write 把一个文件写到 rel。写之前核对现有文件（原路径，或搬家时的新路径）：被手改过就先另存 .local-edit 副本；
// 内容没变不重写；路径变了就搬过去并删掉旧文件。
func (x *exporter) write(key, id, version string, ver int64, rel, head, rest string, archived bool) error {
	content := withMarker(id, version, head, rest)
	e := x.state.Files[key]
	prior := rel
	if e != nil {
		prior = e.Path
	}
	unchanged := false
	for _, p := range []string{prior, rel} {
		old, err := os.ReadFile(x.abs(p))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if p == rel && string(old) == content {
			unchanged = true
		} else if hasLocalEdit(id, string(old)) {
			if err := x.keepLocalEdit(p, old); err != nil {
				return err
			}
		}
		if prior == rel {
			break
		}
	}
	if !unchanged {
		if err := writeFileAtomic(x.abs(rel), []byte(content)); err != nil {
			return err
		}
	}
	if prior != rel {
		if err := os.Remove(x.abs(prior)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		x.prune(prior)
	}
	x.state.Files[key] = &entry{Path: rel, Version: ver, Sig: x.sig, Archived: archived}
	return x.state.save()
}

// keepLocalEdit 把手改过的文件原样另存为同目录的 <文件名>.local-edit，并在状态里记一条。已有副本不覆盖。
func (x *exporter) keepLocalEdit(rel string, data []byte) error {
	cp := rel + ".local-edit"
	if _, err := os.Stat(x.abs(cp)); err == nil {
		cp = fmt.Sprintf("%s-%d", cp, time.Now().UnixNano())
	}
	if err := writeFileAtomic(x.abs(cp), data); err != nil {
		return err
	}
	x.state.Edits = append(x.state.Edits, edit{Path: rel, Copy: cp, At: time.Now().UTC()})
	return nil
}

// drop 在对象被物理删除后删除它的导出文件；文件被手改过则改存为 .local-edit，不丢本人写的内容。
func (x *exporter) drop(key string) error {
	e := x.state.Files[key]
	if e == nil {
		return nil
	}
	old, err := os.ReadFile(x.abs(e.Path))
	switch {
	case err == nil:
		if hasLocalEdit(key[strings.Index(key, "/")+1:], string(old)) {
			if err := x.keepLocalEdit(e.Path, old); err != nil {
				return err
			}
		}
		if err := os.Remove(x.abs(e.Path)); err != nil {
			return err
		}
		x.prune(e.Path)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	delete(x.state.Files, key)
	return x.state.save()
}

// prune 删掉 rel 所在目录里变空的各级目录（到 root 为止，root 本身保留）。
func (x *exporter) prune(rel string) {
	for d := filepath.Dir(rel); d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
		if os.Remove(x.abs(d)) != nil {
			return
		}
	}
}
