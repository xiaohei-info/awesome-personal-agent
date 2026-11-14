package packs

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// Source 说明生效的包来自哪里。
type Source string

const (
	Builtin Source = "builtin"
	Data    Source = "data"
)

// KindRegistry 是事项中心的 kind schema 登记处（items.Kinds 满足它）。
// CheckSchema 只预检不登记，让一个包的 kind 要么全部登记要么都不登记。
type KindRegistry interface {
	Register(kind domain.Kind, schema json.RawMessage) error
	CheckSchema(schema json.RawMessage) error
}

// Rejection 是健康视图里的一条：某个包的某个来源没有通过校验。
type Rejection struct {
	Pack   string
	Source Source
	Hash   string // 被拒绝内容的 hash，同一内容不重复报告
	Reason string
}

// Options 配置加载器。Builtin 是内置包根（packs.FS），DataDir 是 <APA_HOME>/packs，可为空。
type Options struct {
	Builtin fs.FS
	DataDir string
	Kinds   KindRegistry
	// OnReject 在出现新的拒绝时调用一次（同一包的同一内容不重复），由调用方记一条 health.alert（kernel/health 据此统一产出健康类投递）；
	// 加载器自己不写别的模块的数据。
	OnReject func(Rejection)
}

// Loader 持有当前生效的能力包。Load 可重复调用：启动时调一次，之后每次数据目录变化再调（热加载）。
type Loader struct {
	o      Options
	mu     sync.RWMutex
	packs  map[string]*Pack
	src    map[string]Source
	health map[string]Rejection
}

func New(o Options) *Loader {
	return &Loader{o: o, packs: map[string]*Pack{}, src: map[string]Source{}, health: map[string]Rejection{}}
}

type candidate struct {
	src  Source
	fsys fs.FS
}

// Load 重新扫描内置与数据目录。同名包数据目录整体覆盖内置包；校验不通过的包保留上一有效版本
// （没有则退回内置版本），并进健康视图。返回本次新产生的拒绝。
func (l *Loader) Load() ([]Rejection, error) {
	cands, err := l.candidates()
	if err != nil {
		return nil, err
	}
	fresh := l.swap(cands)
	// OnReject 要写账本（拿存储锁）；健康查询在存储事务内读 Health（拿 l.mu）。必须放锁后调用，否则锁序相反会互相死等。
	if l.o.OnReject != nil {
		for _, r := range fresh {
			l.o.OnReject(r)
		}
	}
	return fresh, nil
}

func (l *Loader) swap(cands map[string][]candidate) (fresh []Rejection) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, cs := range cands {
		fresh = append(fresh, l.apply(name, cs)...)
	}
	// 两个来源都没有的包（目录被删）不再生效。kind 的 schema 没有注销接口，保持登记。
	for name := range l.packs {
		if _, ok := cands[name]; !ok {
			delete(l.packs, name)
			delete(l.src, name)
			delete(l.health, name)
		}
	}
	for name := range l.health {
		if _, ok := cands[name]; !ok {
			delete(l.health, name)
		}
	}
	return fresh
}

func (l *Loader) apply(name string, cs []candidate) (fresh []Rejection) {
	rejected := false
	for _, c := range cs {
		p, hash, err := parsePack(c.fsys, name, l.o.Kinds.CheckSchema)
		if err == nil && !l.installed(name, c.src, p.Hash) {
			err = l.install(p, c.src)
		}
		if err == nil {
			if !rejected {
				delete(l.health, name)
			}
			return fresh
		}
		rejected = true
		rej := Rejection{Pack: name, Source: c.src, Hash: hash, Reason: err.Error()}
		if old, seen := l.health[name]; !seen || old.Hash != rej.Hash || old.Reason != rej.Reason {
			fresh = append(fresh, rej)
		}
		l.health[name] = rej
		if _, have := l.packs[name]; have {
			return fresh // 保留上一有效版本
		}
		// 没有上一版本时继续尝试下一个来源（内置包）
	}
	return fresh
}

func (l *Loader) installed(name string, src Source, hash string) bool {
	p, ok := l.packs[name]
	return ok && l.src[name] == src && p.Hash == hash
}

func (l *Loader) install(p *Pack, src Source) error {
	for _, k := range p.Kinds {
		if k.SpecSchema == nil {
			continue
		}
		b, err := json.Marshal(k.SpecSchema)
		if err != nil {
			return fmt.Errorf("kind %s: %w", k.Name, err)
		}
		if err := l.o.Kinds.Register(domain.Kind(k.Name), b); err != nil {
			return err
		}
	}
	l.packs[p.Name] = p
	l.src[p.Name] = src
	return nil
}

// candidates 按优先级列出每个包名的来源：数据目录在前，内置在后。
func (l *Loader) candidates() (map[string][]candidate, error) {
	out := map[string][]candidate{}
	add := func(src Source, fsys fs.FS) error {
		ents, err := fs.ReadDir(fsys, ".")
		if err != nil {
			return err
		}
		for _, e := range ents {
			if e.IsDir() {
				out[e.Name()] = append(out[e.Name()], candidate{src, fsys})
			}
		}
		return nil
	}
	if l.o.DataDir != "" {
		if _, err := os.Stat(l.o.DataDir); err == nil {
			if err := add(Data, os.DirFS(l.o.DataDir)); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if l.o.Builtin != nil {
		if err := add(Builtin, l.o.Builtin); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Get 返回当前生效的包。
func (l *Loader) Get(name string) (*Pack, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.packs[name]
	return p, ok
}

// Source 返回生效包的来源。
func (l *Loader) Source(name string) (Source, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.src[name]
	return s, ok
}

// List 按包名排序返回生效的包。
func (l *Loader) List() []*Pack {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Pack, 0, len(l.packs))
	for _, p := range l.packs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MaterialKinds 返回生效的包声明的材料 kind 及其默认写入策略（C.2）；材料模块经它读，不依赖本包的类型。
func (l *Loader) MaterialKinds() map[string]string {
	out := map[string]string{}
	for _, p := range l.List() {
		for _, m := range p.MaterialKinds {
			out[m.Name] = m.WritePolicy
		}
	}
	return out
}

// Health 返回仍未修复的拒绝，按包名排序，供健康视图展示。
func (l *Loader) Health() []Rejection {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Rejection, 0, len(l.health))
	for _, r := range l.health {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pack < out[j].Pack })
	return out
}

// Validate 预检一个包目录（apa pack validate <目录>），不登记、不改变加载器状态。
func Validate(dir string, checkSchema func(json.RawMessage) error) (*Pack, error) {
	fsys := os.DirFS(dir)
	// 目录名未必等于包名，预检时以 pack.yaml 里的 name 为准。
	files, err := readTree(fsys, ".")
	if err != nil {
		return nil, err
	}
	var head struct {
		Name string `yaml:"name"`
	}
	if raw, ok := files["pack.yaml"]; ok {
		_ = yaml.Unmarshal(raw, &head) // 结构错误由 validatePack 报告
	}
	base := head.Name
	p, err := validatePack(base, files, checkSchema)
	if err != nil {
		return nil, err
	}
	p.Hash = treeHash(files)
	return p, nil
}
