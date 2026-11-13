// Package record 是判断记录的读写与清理（设计 §6.16、§8.6）：判断型每答一道题留一条记录，
// 含逐级的后端、答案或概率、定案者、影子与抽检标记。它是运行队列类数据，不进 Event 日志；
// 用到这次判断的命令在自己的 Event 载荷里带记录 id（见 Ref），`apa why` 据此展开逐级过程。
//
// 记录不含资料正文，只含结构化输入摘要与结果。表结构由账本结构版本 3 在外呼调用器模块里建好
// （outcall_judgment），本包只读写这张表，不改结构。记录写入后不可改（抽检结论的填写不在本包）。
package record

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 定案者、后端、级结果的取值（§6.16）。
const (
	DecidedClassifier = "classifier"
	DecidedChat       = "chat"
	DecidedDefault    = "default"

	BackendClassifier = "classifier"
	BackendChat       = "chat"

	OutcomeDecided   = "decided"
	OutcomeUncertain = "uncertain"
	OutcomeError     = "error"
	OutcomeSkipped   = "skipped"
)

// DefaultRetention 是判断记录的缺省保留期（§6.15）。
const DefaultRetention = 180 * 24 * time.Hour

// Band 是这次判断采用的不确定带。
type Band struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// Level 是一级的结果，按答题顺序排列。
type Level struct {
	Backend           string   `json:"backend"`
	Endpoint          string   `json:"endpoint,omitempty"` // 端点的位置
	Locality          string   `json:"locality,omitempty"` // local 或 external
	Model             string   `json:"model,omitempty"`
	PYes              *float64 `json:"p_yes,omitempty"`      // 分类端点
	Answer            string   `json:"answer,omitempty"`     // 对话端点
	Sufficient        *bool    `json:"sufficient,omitempty"` // 对话端点
	ProbabilitySource string   `json:"probability_source,omitempty"`
	Outcome           string   `json:"outcome"`
	ErrorCode         string   `json:"error_code,omitempty"`
	DurationMs        int64    `json:"duration_ms"`
	Reused            bool     `json:"reused,omitempty"`
}

// Review 是抽检结论；未抽检为 nil。
type Review struct {
	Result string    `json:"result"` // should_see 或 correct
	At     time.Time `json:"at"`
}

// Record 是一条判断记录。
type Record struct {
	ID            string    `json:"id"`
	Use           string    `json:"use"`
	Template      string    `json:"template"`
	Subject       string    `json:"subject"`
	Source        string    `json:"source,omitempty"` // subject 是来源事件时的来源 id
	Fingerprint   string    `json:"fingerprint"`
	Band          Band      `json:"band"`
	Levels        []Level   `json:"levels"`
	DecidedBy     string    `json:"decided_by"`
	Answer        string    `json:"answer"`
	LowConfidence bool      `json:"low_confidence,omitempty"`
	Shadow        bool      `json:"shadow,omitempty"`
	Review        *Review   `json:"review,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Ref 是命令 Event 载荷里指向判断记录的部分：记录过了保留期后，`apa why` 只剩这里的结论。
// 使用判断结果的命令（入站处置、初筛结果、投递推迟、跟进推迟）把它并进自己的载荷。
type Ref struct {
	Judgment  string `json:"judgment"`
	Answer    string `json:"answer,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
}

// RefOf 取一条记录的引用。
func RefOf(r Record) Ref { return Ref{Judgment: r.ID, Answer: r.Answer, DecidedBy: r.DecidedBy} }

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the judgment record")
}

func oneOf(v string, set ...string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// Validate 检查记录的结构：必填字段、枚举取值，以及定案者与级结果自洽。
// 内容不含资料正文由类型保证：这里没有任何自由文本的正文字段。
func Validate(r Record) error {
	for name, v := range map[string]string{"id": r.ID, "use": r.Use, "template": r.Template, "subject": r.Subject, "fingerprint": r.Fingerprint, "answer": r.Answer} {
		if strings.TrimSpace(v) == "" {
			return invalid(name + " is empty")
		}
	}
	if r.CreatedAt.IsZero() {
		return invalid("created_at is zero")
	}
	if r.Band.Low < 0 || r.Band.High > 1 || r.Band.Low > r.Band.High {
		return invalid("band must satisfy 0 <= low <= high <= 1")
	}
	if !oneOf(r.DecidedBy, DecidedClassifier, DecidedChat, DecidedDefault) {
		return invalid("decided_by must be classifier, chat or default")
	}
	if r.LowConfidence != (r.DecidedBy == DecidedDefault) {
		return invalid("low_confidence must be true exactly when decided_by is default")
	}
	decided := ""
	for i, l := range r.Levels {
		if !oneOf(l.Backend, BackendClassifier, BackendChat) {
			return invalid(fmt.Sprintf("levels[%d].backend must be classifier or chat", i))
		}
		if !oneOf(l.Outcome, OutcomeDecided, OutcomeUncertain, OutcomeError, OutcomeSkipped) {
			return invalid(fmt.Sprintf("levels[%d].outcome is not recognised", i))
		}
		if l.PYes != nil && (*l.PYes < 0 || *l.PYes > 1) {
			return invalid(fmt.Sprintf("levels[%d].p_yes must be within [0, 1]", i))
		}
		if l.DurationMs < 0 {
			return invalid(fmt.Sprintf("levels[%d].duration_ms is negative", i))
		}
		if l.Outcome == OutcomeDecided && decided == "" {
			decided = l.Backend
		}
	}
	// 影子打分只记分、不定案（§8.10）：定案者与级结果的对应只对真正定案的记录检查。
	if !r.Shadow && (decided != r.DecidedBy && !(r.DecidedBy == DecidedDefault && decided == "")) {
		return invalid("decided_by does not match the first level that decided (default means no level decided)")
	}
	if r.Review != nil && !oneOf(r.Review.Result, "should_see", "correct") {
		return invalid("review.result must be should_see or correct")
	}
	return nil
}

// Put 写入一条记录。id 已存在返回 conflict：记录写入后不可改。
func Put(tx *store.Tx, r Record) error {
	if err := Validate(r); err != nil {
		return err
	}
	levels, err := json.Marshal(r.Levels)
	if err != nil {
		return err
	}
	band, err := json.Marshal(r.Band)
	if err != nil {
		return err
	}
	var review string
	var reviewedAt int64
	if r.Review != nil {
		review, reviewedAt = r.Review.Result, r.Review.At.UnixNano()
	}
	_, err = tx.Exec(`INSERT INTO outcall_judgment
		(id, use, template, subject, source, fingerprint, band, levels, decided_by, answer, low_confidence, shadow, review, reviewed_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Use, r.Template, r.Subject, r.Source, r.Fingerprint, string(band), string(levels), r.DecidedBy, r.Answer,
		b2i(r.LowConfidence), b2i(r.Shadow), review, reviewedAt, r.CreatedAt.UnixNano())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return domain.NewError(domain.CodeConflict, "judgment record "+r.ID+" already exists", "records are immutable once written")
		}
		return err
	}
	return nil
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

const cols = `id, use, template, subject, source, fingerprint, band, levels, decided_by, answer, low_confidence, shadow, review, reviewed_at, created_at`

func scan(rows *store.Rows) (Record, error) {
	var (
		r                          Record
		band, levels, review       string
		low, shadow, rat, createdN int64
	)
	if err := rows.Scan(&r.ID, &r.Use, &r.Template, &r.Subject, &r.Source, &r.Fingerprint, &band, &levels, &r.DecidedBy, &r.Answer,
		&low, &shadow, &review, &rat, &createdN); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(band), &r.Band); err != nil {
		return r, fmt.Errorf("judgment %s band: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(levels), &r.Levels); err != nil {
		return r, fmt.Errorf("judgment %s levels: %w", r.ID, err)
	}
	r.LowConfidence, r.Shadow, r.CreatedAt = low != 0, shadow != 0, time.Unix(0, createdN).UTC()
	if review != "" {
		r.Review = &Review{Result: review, At: time.Unix(0, rat).UTC()}
	}
	return r, nil
}

func query(tx *store.Tx, tail string, args ...any) ([]Record, error) {
	rows, err := tx.Query(`SELECT `+cols+` FROM outcall_judgment `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get 按 id 读一条记录；不存在（含已过保留期被清理）返回 not_found。
func Get(tx *store.Tx, id string) (Record, error) {
	rs, err := query(tx, `WHERE id = ?`, id)
	if err != nil {
		return Record{}, err
	}
	if len(rs) == 0 {
		return Record{}, domain.NewError(domain.CodeNotFound, "judgment record "+id+" not found", "the record may be past its retention period")
	}
	return rs[0], nil
}

// Filter 是列表条件，空字段不筛。After 是上一页最后一条记录的 id：返回按 (created_at, id) 排在它之后的。
type Filter struct {
	Subject, Use, Template, Source, After string
	Limit                                 int
	Since                                 time.Time // 非零时只返回 created_at 不早于它的记录
}

// 列表分页上限。
const (
	DefaultLimit = 100
	MaxLimit     = 500
)

// List 按 (created_at, id) 升序返回一页；more 为真表示还有下一页（取最后一条的 id 作为 After）。
func List(tx *store.Tx, f Filter) (out []Record, more bool, err error) {
	if f.Limit < 0 || f.Limit > MaxLimit {
		return nil, false, invalid("limit must be within 1..500")
	}
	if f.Limit == 0 {
		f.Limit = DefaultLimit
	}
	var where []string
	var args []any
	for col, v := range map[string]string{"subject": f.Subject, "use": f.Use, "template": f.Template, "source": f.Source} {
		if v != "" {
			where, args = append(where, col+" = ?"), append(args, v)
		}
	}
	if !f.Since.IsZero() {
		where, args = append(where, "created_at >= ?"), append(args, f.Since.UnixNano())
	}
	if f.After != "" {
		a, err := Get(tx, f.After)
		if err != nil {
			return nil, false, domain.NewError(domain.CodeInvalidRequest, "after is not a known judgment record id", "pass the last id of the previous page")
		}
		where = append(where, "(created_at > ? OR (created_at = ? AND id > ?))")
		args = append(args, a.CreatedAt.UnixNano(), a.CreatedAt.UnixNano(), a.ID)
	}
	tail := ""
	if len(where) > 0 {
		tail = "WHERE " + strings.Join(where, " AND ") + " "
	}
	rs, err := query(tx, tail+`ORDER BY created_at, id LIMIT ?`, append(args, f.Limit+1)...)
	if err != nil {
		return nil, false, err
	}
	if len(rs) > f.Limit {
		return rs[:f.Limit], true, nil
	}
	return rs, false, nil
}

// SetReview 填抽检结论（§8.10）：只写未抽检的记录，已有结论返回 conflict；判断本身的字段不变。
// 谁能填由调用的命令授权（只在 Web），本函数不做授权。
func SetReview(tx *store.Tx, id, result string, at time.Time) error {
	if !oneOf(result, "should_see", "correct") {
		return invalid("review.result must be should_see or correct")
	}
	n, err := tx.Exec(`UPDATE outcall_judgment SET review = ?, reviewed_at = ? WHERE id = ? AND review = ''`, result, at.UnixNano(), id)
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := Get(tx, id); err != nil {
			return err
		}
		return domain.NewError(domain.CodeConflict, "judgment record "+id+" is already reviewed", "a spot-check result is written once")
	}
	return nil
}

// DeleteBefore 删除 created_at 早于 cutoff 的记录（保留期清理），返回删除条数。Event 与事项不受影响。
func DeleteBefore(tx *store.Tx, cutoff time.Time) (int64, error) {
	return tx.Exec(`DELETE FROM outcall_judgment WHERE created_at < ?`, cutoff.UnixNano())
}

// DeleteBySubject 删除指向这些对象的记录（物理删除来源事件或事项时一并删除，§6.16）。
func DeleteBySubject(tx *store.Tx, subjects ...string) (int64, error) {
	var n int64
	for _, s := range subjects {
		k, err := tx.Exec(`DELETE FROM outcall_judgment WHERE subject = ?`, s)
		if err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}
