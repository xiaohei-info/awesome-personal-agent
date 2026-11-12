package ledgerfixture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// PayloadHashMethod 说明 event_payload_sha256 的算法，验收端按同一方式重算。
const PayloadHashMethod = "sha256 hex of the Event payload JSON text exactly as stored in eventlog_event.payload (compact JSON, UTF-8, no trailing newline); the API returns the same bytes"

// Manifest 是夹具根目录 manifest.json 的内容：全部期望基线。键名与验收文档（ACT-ENV-08）一致。
type Manifest struct {
	Schema            string            `json:"schema"`
	SchemaLedger      int               `json:"schema_ledger"`
	Tables            map[string]int    `json:"tables"`
	EventMaxSeq       int64             `json:"event_max_seq"`
	EventPayloadSHA   map[string]string `json:"event_payload_sha256"`
	PayloadHashMethod string            `json:"payload_hash_method"`
	Backlog           Backlog           `json:"backlog"`
	StructuredHits    int               `json:"structured_rule_hits"` // 规则 then: proposed 可直接处置、且不能挂靠的积压事件数
	Attachable        int               `json:"attachable"`           // 规则命中且能挂靠到已有事项的积压事件数
	StructuredIDs     []string          `json:"structured_rule_hit_ids"`
	AttachableIDs     []string          `json:"attachable_ids"`
	ScoreMarkers      Markers           `json:"score_markers"`
	OldCascadeIgnored OldCascade        `json:"old_cascade_ignored"`
	RuleIgnored       []string          `json:"rule_ignored"`
	UserIgnored       []string          `json:"user_ignored"`
	OutsideWindow     []string          `json:"outside_window"`
	PolicyVersions    map[string]string `json:"policy_versions"` // 策略版本 id → 文档 sha256
	PolicyDetails     []PolicyVersion   `json:"policy_version_details"`
	RuleModes         RuleModes         `json:"rule_modes"`
	Tokens            map[string]string `json:"tokens"`
	WebSecret         string            `json:"web_password"`
	SessionCookie     string            `json:"session_cookie"`
	UserPack          UserPack          `json:"user_pack"`
	Sources           []string          `json:"sources"`
	WaitingItems      []string          `json:"waiting_items"`
	Proposed          []string          `json:"proposed_events"`
	FrozenCommit      string            `json:"frozen_commit,omitempty"`
	MigrationFiles    []string          `json:"migration_files,omitempty"` // 迁移所在源文件（定位用；整文件会随后续版本的迁移追加而变，不能逐字节比对）
	// MigrationChecksums 是冻结版本含有的每一步迁移的校验和（module/version/checksum）。迁移 SQL 一经冻结不再变，
	// 这才是可核对的冻结证据：从当前源码重算必须逐条相等（见 TestManifestMigrationChecksumsMatchSource）。
	MigrationChecksums []string `json:"migration_checksums,omitempty"`
	Binary             string   `json:"binary,omitempty"` // 回退用的旧版本主程序：环境变量名
	Plugin             string   `json:"plugin,omitempty"`
}

type Backlog struct {
	Total    int            `json:"total"`
	Unjudged map[string]int `json:"unjudged"` // 原因 model_unavailable / undecided / none（原因为空）
	Expired  int            `json:"expired"`
	// Health 是 `apa health` 的 unjudged_backlog 信号对同一份账本报出的数：原因为空的与 model_unavailable 合计（两者都在模型恢复后自动重判）。
	Health map[string]int `json:"health"`
}

func (b *Backlog) add(disp, reason string) {
	if b.Health == nil {
		b.Health = map[string]int{}
	}
	if disp == "expired" {
		b.Expired++
		b.Health["expired"]++
		return
	}
	b.Health["count"]++
	if reason == "undecided" {
		b.Health["undecided"]++
	} else {
		b.Health["model_unavailable"]++
	}
	if b.Unjudged == nil {
		b.Unjudged = map[string]int{}
	}
	b.Unjudged[orDefault(reason, "none")]++
}

type Markers map[string]*MarkerCount

type MarkerCount struct {
	Count    int            `json:"count"`
	BySource map[string]int `json:"by_source"`
}

func (m *Markers) add(marker, source string) {
	if *m == nil {
		*m = Markers{}
	}
	c := (*m)[marker]
	if c == nil {
		c = &MarkerCount{BySource: map[string]int{}}
		(*m)[marker] = c
	}
	c.Count++
	c.BySource[source]++
}

type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type OldCascade struct {
	Window   Window         `json:"window"`
	Count    int            `json:"count"`
	BySource map[string]int `json:"by_source"`
}

type PolicyVersion struct {
	ID             int    `json:"id"`
	SHA256         string `json:"sha256"`
	ModelEndpoint  bool   `json:"model_endpoint"`
	ThresholdJudge bool   `json:"threshold_judge"`
}

type ModeChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type RuleModes struct {
	Current                 map[string]string     `json:"current"`
	Untouched               []string              `json:"untouched"`
	OwnerSwitched           map[string]ModeChange `json:"owner_switched"`
	UserPackExplicitObserve []string              `json:"user_pack_explicit_observe"`
	OffByDefault            []string              `json:"off_by_default"`
}

type UserPack struct {
	Name                string   `json:"name"`
	Overrides           string   `json:"overrides"`
	ConfidenceThreshold float64  `json:"confidence_threshold"`
	Files               []string `json:"files"`
}

// baseline 在写入事务内从账本读出需要「从数据里数出来」的基线：各表行数、最大 Event 序号与每条载荷的哈希。
// 读的是真实写进去的行，不是生成器的计划，所以清单与账本不会漂移。
func (f *fx) baseline() {
	m := f.m
	m.Tables = map[string]int{}
	r, err := f.tx.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'material_search_%' ORDER BY name`)
	if err != nil {
		panic(err)
	}
	var names []string
	for r.Next() {
		var n string
		if err := r.Scan(&n); err != nil {
			panic(err)
		}
		names = append(names, n)
	}
	r.Close()
	for _, n := range names {
		var c int64
		q, err := f.tx.Query("SELECT COUNT(*) FROM " + n) // 表名来自 sqlite_master，不是外部输入
		if err != nil {
			panic(err)
		}
		q.Next()
		if err := q.Scan(&c); err != nil {
			panic(err)
		}
		q.Close()
		m.Tables[n] = int(c)
	}
	m.EventPayloadSHA = map[string]string{}
	q, err := f.tx.Query(`SELECT seq, payload FROM eventlog_event ORDER BY seq`)
	if err != nil {
		panic(err)
	}
	defer q.Close()
	for q.Next() {
		var seq int64
		var p string
		if err := q.Scan(&seq, &p); err != nil {
			panic(err)
		}
		m.EventPayloadSHA[fmt.Sprint(seq)] = sha(p)
		m.EventMaxSeq = seq
	}
	m.PayloadHashMethod = PayloadHashMethod
	m.StructuredHits, m.Attachable = len(m.StructuredIDs), len(m.AttachableIDs)
	m.PolicyVersions = map[string]string{}
	for _, p := range m.PolicyDetails {
		m.PolicyVersions[fmt.Sprint(p.ID)] = p.SHA256
	}
}

// migrationFiles 返回登记了结构版本 ≤ version 迁移的模块所在的源文件（仓库相对路径）。
func migrationFiles(version int) []string {
	_, self, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", ".."))
	reg, err := RegistryAt(version)
	if err != nil {
		panic(err)
	}
	want := map[string]bool{}
	for _, m := range reg.Modules() {
		want[`store.Module{Name: "`+m.Name+`"`] = true
	}
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "tests", ".omac":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for k := range want {
			if strings.Contains(string(b), k) {
				rel, _ := filepath.Rel(root, p)
				files = append(files, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	sort.Strings(files)
	return files
}
