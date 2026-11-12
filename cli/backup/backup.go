// Package backup 实现主机本地的 `apa backup create|verify|restore` 与升级前自动快照（设计 §16.5，附录 B.1）。
// 备份是一个 0700 目录：账本用在线备份导出的单文件副本，加配置与数据文件，外带 manifest.json
// （APA 版本、各模块数据结构版本、各表行数、文件哈希）。回退只有"旧二进制 + 升级前快照 + restore"一条路，不提供逆向迁移。
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ManifestFile 是备份目录里的清单文件名；它描述备份，不属于数据目录，恢复时不落入目标。
const ManifestFile = "manifest.json"

const manifestFormat = 1

// 备份范围（§16.5）：不含 runs/、run-credentials/、logs/、backups/。
var (
	plainFiles = []string{"config.yaml", "sources.json"}
	plainDirs  = []string{"blobs", "packs", "declarations"}
	secretDirs = []string{"sources", "webhook-keys"} // 来源凭据与接收密钥明文；--no-secrets 不含
)

// Manifest 是 manifest.json。
type Manifest struct {
	Format     int    `json:"format"`
	APAVersion string `json:"apa_version"`
	CreatedAt  string `json:"created_at"`
	NoSecrets  bool   `json:"no_secrets"`
	Schema     struct {
		Ledger  int            `json:"ledger"`
		Modules map[string]int `json:"modules"`
	} `json:"schema"`
	Tables      map[string]int64  `json:"tables"`
	EventMaxSeq int64             `json:"event_max_seq"`
	Files       map[string]string `json:"files"` // 相对路径 → sha256
	Dirs        []string          `json:"dirs"`  // 含空目录，恢复时原样建出

	Dir string `json:"-"` // 备份目录（Create 返回时填）
}

// Options 是 Create 的参数。
type Options struct {
	Out       string // 为空时用 <home>/backups/backup-<时刻>
	NoSecrets bool
	core      bool // 升级前快照：只含账本与配置
}

func conflict(why, next string) error { return domain.NewError(domain.CodeConflict, why, next) }
func usage(why, next string) error    { return domain.NewError(domain.CodeInvalidRequest, why, next) }

func apaVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

// emptyTarget 确认 dir 不存在或是空目录；否则返回冲突错误。
func emptyTarget(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return conflict("the target is not a directory: "+dir, "give a new or empty directory")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return conflict("the target directory is not empty: "+dir+"; backups are only written or restored to an empty directory", "give a new or empty directory; an APA home in use is never overwritten")
	}
	return nil
}

// Create 导出 home 的备份。服务运行中也可执行：账本经在线备份取一致快照，不迁移、不改源账本。
func Create(ctx context.Context, home string, o Options, reg *store.Registry, now time.Time) (m Manifest, err error) {
	l := config.LayoutOf(home)
	if _, err := os.Stat(l.Ledger); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, domain.NewError(domain.CodeNotFound, "no ledger in "+home, "set APA_HOME to the data directory to back up")
		}
		return m, err
	}
	out := o.Out
	if out == "" {
		out = filepath.Join(l.Home, "backups", "backup-"+now.UTC().Format("20060102T150405Z"))
	}
	if out, err = filepath.Abs(out); err != nil {
		return m, err
	}
	if err := emptyTarget(out); err != nil {
		return m, err
	}
	_, statErr := os.Lstat(out) // 目标是否原本就在（空目录）
	if err := os.MkdirAll(out, 0o700); err != nil {
		return m, err
	}
	defer func() {
		if err != nil { // 失败不留半个备份（含明文密钥）
			if statErr == nil { // 原本就在：只清内容，保留目录本身
				clearDir(out)
			} else {
				os.RemoveAll(out)
			}
		}
	}()
	if err := os.Chmod(out, 0o700); err != nil {
		return m, err
	}
	ledger := filepath.Join(out, store.LedgerFile)
	if err := store.BackupFile(ctx, home, ledger); err != nil {
		return m, err
	}
	if err := os.Chmod(ledger, 0o600); err != nil {
		return m, err
	}
	files, dirs := plainFiles, plainDirs
	if o.core {
		files, dirs = []string{"config.yaml"}, nil
	} else if !o.NoSecrets {
		dirs = append(append([]string{}, dirs...), secretDirs...)
	}
	for _, f := range files {
		if err := copyFile(filepath.Join(home, f), filepath.Join(out, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return m, err
		}
	}
	for _, d := range dirs {
		if err := copyTree(filepath.Join(home, d), filepath.Join(out, d)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return m, err
		}
	}
	info, err := store.Inspect(ledger, reg)
	if err != nil {
		return m, err
	}
	m = Manifest{Format: manifestFormat, APAVersion: apaVersion(), CreatedAt: now.UTC().Format(time.RFC3339), NoSecrets: o.NoSecrets,
		Tables: info.Tables, EventMaxSeq: info.EventMaxSeq, Dir: out}
	m.Schema.Ledger, m.Schema.Modules = info.Ledger, info.Modules
	if m.Files, m.Dirs, err = scan(out); err != nil {
		return m, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	return m, os.WriteFile(filepath.Join(out, ManifestFile), append(b, '\n'), 0o600)
}

func clearDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// scan 返回目录下除清单外的全部文件哈希与全部子目录。遇到符号链接等非常规文件即报错：备份只含普通文件。
func scan(root string) (files map[string]string, dirs []string, err error) {
	files = map[string]string{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case rel == ".":
		case d.IsDir():
			dirs = append(dirs, filepath.ToSlash(rel))
		case !d.Type().IsRegular():
			return fmt.Errorf("backup: %s is not a regular file", rel)
		case rel != ManifestFile:
			h, err := hashFile(p)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = h
		}
		return nil
	})
	sort.Strings(dirs)
	return
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("backup: %s is not a regular file", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return writeFrom(in, dst, fi.Mode().Perm())
}

func writeFrom(in io.Reader, dst string, perm os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm) // 不受 umask 影响
}

func copyTree(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

// Report 是 Verify 通过时的结果。
type Report struct {
	Manifest         Manifest
	Files            int
	HashesOK         bool
	IntegrityOK      bool
	EventsContinuous bool
	EventMaxSeq      int64
}

func readManifest(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return m, domain.NewError(domain.CodeInternal, "backup manifest unreadable: "+err.Error(), "point at a directory made by `apa backup create`")
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Format != manifestFormat || m.Files == nil {
		return m, domain.NewError(domain.CodeInternal, "backup manifest is malformed or from an unknown format", "the backup cannot be used; make a new one")
	}
	for rel := range m.Files {
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return m, domain.NewError(domain.CodeInternal, "backup manifest lists a path outside the backup: "+rel, "the backup cannot be used; make a new one")
		}
	}
	m.Dir = dir
	return m, nil
}

// Verify 核对清单里的文件哈希（含多出与缺少的文件），对账本副本做完整性检查，并检查 Event 序号连续。
// 任一项不过都列在错误里（退出码 1）。账本里有 reg 不认识的迁移同样不过。
func Verify(dir string, reg *store.Registry) (Report, error) {
	m, err := readManifest(dir)
	if err != nil {
		return Report{}, err
	}
	var problems []string
	files, _, err := scan(dir)
	if err != nil {
		problems = append(problems, err.Error())
	}
	hashesOK := true
	for rel, want := range m.Files {
		got, ok := files[rel]
		switch {
		case !ok:
			problems = append(problems, "missing file: "+rel)
		case got != want:
			problems = append(problems, "hash mismatch: "+rel)
		default:
			continue
		}
		hashesOK = false
	}
	for rel := range files {
		if _, ok := m.Files[rel]; !ok {
			problems = append(problems, "unlisted file: "+rel)
			hashesOK = false
		}
	}
	rep := Report{Manifest: m, Files: len(m.Files), HashesOK: hashesOK}
	if _, ok := m.Files[store.LedgerFile]; !ok {
		problems = append(problems, "manifest lists no ledger")
	} else if info, err := store.Inspect(filepath.Join(dir, store.LedgerFile), reg); err != nil {
		problems = append(problems, "ledger: "+err.Error())
	} else {
		rep.IntegrityOK, rep.EventMaxSeq, rep.EventsContinuous = true, info.EventMaxSeq, info.EventContinuous
		if !info.EventContinuous {
			problems = append(problems, "event sequence is not continuous")
		}
		if info.Ledger != m.Schema.Ledger || info.EventMaxSeq != m.EventMaxSeq || !sameTables(info.Tables, m.Tables) {
			problems = append(problems, "manifest does not match the ledger copy")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return rep, domain.NewError(domain.CodeInternal, "backup verification failed: "+strings.Join(problems, "; "), "do not restore from this backup; make a new one")
	}
	return rep, nil
}

func sameTables(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Result 是 Restore 的结果。
type Result struct {
	Home         string
	SchemaLedger int
	NoSecrets    bool
	Notices      []string
}

// Restore 把备份恢复到空目录 target（不存在则新建）。目标非空、备份校验不过、
// 备份的账本结构版本高于当前二进制时一律拒绝，且不在目标留下任何文件。
func Restore(dir, target string, reg *store.Registry) (res Result, err error) {
	m, err := readManifest(dir)
	if err != nil {
		return Result{}, err
	}
	if m.Schema.Ledger > reg.SchemaLedger() {
		return Result{}, usage(fmt.Sprintf("the backup's ledger structure version %d is newer than this binary's %d", m.Schema.Ledger, reg.SchemaLedger()),
			"restore it with the newer APA binary that made it")
	}
	if _, err := Verify(dir, reg); err != nil {
		return Result{}, err
	}
	if target, err = filepath.Abs(target); err != nil {
		return Result{}, err
	}
	if err := emptyTarget(target); err != nil {
		return Result{}, err
	}
	_, statErr := os.Lstat(target) // 目标是否原本就在（空目录）
	defer func() {
		if err != nil {
			if statErr == nil { // 原本就在：只清内容，保留目录本身
				clearDir(target)
			} else {
				os.RemoveAll(target)
			}
		}
	}()
	if err = os.MkdirAll(target, 0o755); err != nil {
		return Result{}, err
	}
	for _, d := range m.Dirs {
		if err = os.MkdirAll(filepath.Join(target, filepath.FromSlash(d)), 0o755); err != nil {
			return Result{}, err
		}
	}
	for rel := range m.Files {
		perm := os.FileMode(0o644)
		if top, _, _ := strings.Cut(rel, "/"); top == "sources" || top == "webhook-keys" || rel == store.LedgerFile {
			perm = 0o600
		}
		dst := filepath.Join(target, filepath.FromSlash(rel))
		in, oerr := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if oerr != nil {
			return Result{}, oerr
		}
		err = writeFrom(in, dst, perm)
		in.Close()
		if err != nil {
			return Result{}, err
		}
	}
	if err = config.LayoutOf(target).Ensure(); err != nil { // 补齐没进备份的目录（runs/、logs/ 等；密钥目录 0700）
		return Result{}, err
	}
	res = Result{Home: target, SchemaLedger: m.Schema.Ledger, NoSecrets: m.NoSecrets}
	if m.NoSecrets {
		res.Notices = []string{
			"this backup holds no source credentials: re-authenticate every source (`apa source auth <id>` or the tool's own login)",
			"this backup holds no webhook receive keys: rotate each webhook source's key; the old keys are rejected",
		}
	}
	return res, nil
}

// PreUpgradeSnapshot 在账本有待办迁移时，于 <home>/backups/ 下留一份只含账本与配置的升级前快照并返回其目录；
// 没有账本或没有待办迁移时返回空串。快照失败则返回错误，调用方不得继续迁移。
func PreUpgradeSnapshot(ctx context.Context, home string, reg *store.Registry, now time.Time) (string, error) {
	pending, err := store.Pending(home, reg)
	if err != nil || !pending {
		return "", err
	}
	base := filepath.Join(home, "backups", "pre-upgrade-"+now.UTC().Format("20060102T150405Z"))
	out := base
	for n := 2; ; n++ { // 同一秒再次启动不覆盖前一份
		if _, err := os.Lstat(out); err != nil { // 不存在（或父路径不可用，留给 Create 报错）
			break
		}
		out = fmt.Sprintf("%s-%d", base, n)
	}
	m, err := Create(ctx, home, Options{Out: out, core: true}, reg, now)
	if err != nil {
		return "", fmt.Errorf("pre-upgrade snapshot failed, the ledger was not migrated: %w", err)
	}
	return m.Dir, nil
}
