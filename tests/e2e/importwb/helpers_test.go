//go:build e2e

package importwb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const waitMax = 60 * time.Second

type fx struct {
	*baseline.Env
	t      *testing.T
	offset int64 // 测试时钟已推进的秒数（内核时间 = 真实时间 + offset）
	mode   string
	seen   map[string]bool // 已读过的 STUB_HARNESS 保存目录
	cookie string          // 导入请求用的 Web 登录会话（缓存：每次登录都会多一条 auth.login 事件）
	csrf   string
}

func setup(t *testing.T) *fx {
	t.Helper()
	f := &fx{Env: baseline.Setup(t), t: t, seen: map[string]bool{}}
	f.relogin()
	return f
}

// relogin 重新取导入请求用的 Web 会话：测试时钟推进数日或内核重启后原会话可能已失效。
func (f *fx) relogin() { f.cookie, f.csrf = f.webSession() }

func (f *fx) now() time.Time { return time.Now().Add(time.Duration(f.offset) * time.Second) }

// advance 推进测试时钟并记账；推进得久了，登录会话已过期。
func (f *fx) advance(sec int64) {
	f.TCAdvance(sec)
	f.offset += sec
	f.WebLogin()
	f.relogin()
}

func (f *fx) step(name string, fn func(t *testing.T)) {
	f.t.Helper()
	parent := f.t
	ok := parent.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续动作不得开始", name)
	}
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) web(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) api(token, method, path string, want int) string {
	f.t.Helper()
	r := f.APIRaw(method, path, "", token)
	if r.Code != want {
		f.t.Fatalf("API %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%.500s", err, s)
	}
	return v
}

// ---- 导入请求：Web 页面调用的同一套 HTTP API，请求体是 application/x-ndjson ----

type resp struct {
	Code int
	Body string
}

// importHTTP 发一次 POST /v1/import。cookieAuth 为真时用本人的 Web 登录会话（带 CSRF），否则用 bearer 凭据；hdr 是附加请求头。
func (f *fx) importHTTP(source, query, body, bearer string, hdr map[string]string) resp {
	f.t.Helper()
	r, err := f.importHTTPErr(source, query, body, bearer, hdr)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// importHTTPErr 同 importHTTP，但传输失败（如内核被杀）作为 error 返回，可在测试 goroutine 之外调用。
func (f *fx) importHTTPErr(source, query, body, bearer string, hdr map[string]string) (resp, error) {
	path := "/v1/import?source=" + source
	if query != "" {
		path += "&" + query
	}
	req, err := http.NewRequest("POST", f.URL+path, strings.NewReader(body))
	if err != nil {
		return resp{}, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else {
		req.Header.Set("Cookie", "apa_session="+f.cookie)
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return resp{}, fmt.Errorf("POST %s: %w", path, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, string(b)}, nil
}

// importWeb 是本人在 Web 登录下的导入（不带授权头）。
func (f *fx) importWeb(source, query, body string, hdr map[string]string) resp {
	f.t.Helper()
	return f.importHTTP(source, query, body, "", hdr)
}

// webSession 登录一次新的 Web 会话（口令登录，同 baseline），返回会话 Cookie 值与 CSRF 令牌。
func (f *fx) webSession() (cookie, csrf string) {
	f.t.Helper()
	r, err := http.Post(f.URL+"/v1/auth/login", "application/json", strings.NewReader(`{"password":"`+baseline.Password+`"}`))
	if err != nil {
		f.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	for _, c := range r.Cookies() {
		if c.Name == "apa_session" {
			cookie = c.Value
		}
	}
	if r.StatusCode != 200 || json.Unmarshal(b, &out) != nil || cookie == "" || out.CSRF == "" {
		f.t.Fatalf("Web 登录失败: %d %s", r.StatusCode, b)
	}
	return cookie, out.CSRF
}

// ---- 导入响应 ----

type line struct {
	Line       int    `json:"line"`
	ExternalID string `json:"external_id"`
	Outcome    string `json:"outcome"`
	Object     string `json:"object"`
	Code       string `json:"code"`
	Why        string `json:"why"`
}

type importResult struct {
	Batch    string `json:"batch"`
	Results  []line `json:"results"`
	Created  int    `json:"created"`
	Existing int    `json:"existing"`
	Invalid  int    `json:"invalid"`
}

func (f *fx) mustImport(source, query, body string) importResult {
	f.t.Helper()
	r := f.importWeb(source, query, body, nil)
	if r.Code != 200 {
		f.t.Fatalf("POST /v1/import?source=%s&%s: HTTP %d %s", source, query, r.Code, r.Body)
	}
	return decode[importResult](f.t, r.Body)
}

// ---- 账本（只读 sqlite，等价验收文档的 `sqlite3 -readonly`） ----

func (f *fx) ledger() *sql.DB {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Home, "ledger.db")+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { db.Close() })
	return db
}

// tableCounts 是全部表（不含 sqlite_ 内部表与全文索引影子表）的行数。
func (f *fx) tableCounts() map[string]int {
	f.t.Helper()
	db := f.ledger()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'material_search_%' ORDER BY name`)
	if err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			f.t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	out := map[string]int{}
	for _, n := range names {
		var c int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + n).Scan(&c); err != nil { // 表名来自 sqlite_master
			f.t.Fatal(err)
		}
		out[n] = c
	}
	return out
}

func (f *fx) maxSeq() int64 {
	f.t.Helper()
	var n sql.NullInt64
	if err := f.ledger().QueryRow(`SELECT MAX(seq) FROM eventlog_event`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n.Int64
}

func (f *fx) queryStrings(q string, args ...any) []string {
	f.t.Helper()
	rows, err := f.ledger().Query(q, args...)
	if err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (f *fx) until(what string, cond func() bool) {
	f.t.Helper()
	if !f.WaitFor(waitMax, cond) {
		f.t.Fatalf("%v 内没有等到：%s\n%s", waitMax, what, f.ServiceLog())
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("jsonLine: %v", err))
	}
	return string(b)
}

func itoa(n int64) string { return fmt.Sprint(n) }

// ---- 审计 Event（Web 登录看得到载荷） ----

type auditEv struct {
	Seq        int64     `json:"seq"`
	Type       string    `json:"type"`
	Object     string    `json:"object"`
	At         time.Time `json:"at"`
	Imported   string    `json:"imported"`
	RecordedAt string    `json:"recorded_at"`
	Grant      string    `json:"grant"`
	Actor      struct {
		OnBehalfOf string `json:"on_behalf_of"`
	} `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

func (f *fx) audit(query string) []auditEv {
	f.t.Helper()
	return decode[struct {
		Events []auditEv `json:"events"`
	}](f.t, f.web("GET", "/v1/audit/events?limit=500&"+query, "", 200)).Events
}

// ---- 事项 ----

type itemView struct {
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	Project   string    `json:"project"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Origin    *struct {
		Source string `json:"source"`
		Ref    string `json:"ref"`
	} `json:"origin"`
	Imported *struct {
		Batch string    `json:"batch"`
		At    time.Time `json:"at"`
	} `json:"imported"`
}

type itemShow struct {
	Item     itemView `json:"item"`
	Progress []struct {
		At        time.Time `json:"at"`
		On        string    `json:"on"`
		Text      string    `json:"text"`
		Precision string    `json:"precision"`
	} `json:"progress"`
}

// byOrigin 按（来源，外部 id）取事项，走 CLI 的 --origin-source/--origin-ref；ref 为空取该来源的全部。
func (f *fx) byOrigin(source, ref string) []itemView {
	f.t.Helper()
	args := []string{"item", "list", "--origin-source", "import:" + source}
	if ref != "" {
		args = append(args, "--origin-ref", ref)
	}
	return decode[struct {
		Items []itemView `json:"items"`
	}](f.t, f.apa(f.OBS, 0, args...).Stdout).Items
}

func (f *fx) show(id string) itemShow {
	f.t.Helper()
	return decode[itemShow](f.t, f.apa(f.OBS, 0, "item", "show", id).Stdout)
}

type batchView struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Created  int    `json:"created"`
	Existing int    `json:"existing"`
	Invalid  int    `json:"invalid"`
	Actor    string `json:"actor"`
	Grant    string `json:"grant"`
}

func (f *fx) batches() []batchView {
	f.t.Helper()
	return decode[struct {
		Batches []batchView `json:"batches"`
	}](f.t, f.web("GET", "/v1/imports", "", 200)).Batches
}

// eventCount 数某类型的事件行数（直接读账本）。
func (f *fx) eventCount(where string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.ledger().QueryRow(`SELECT COUNT(*) FROM eventlog_event WHERE `+where, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// ---- 计划文件里的单条事项：created、一条进展、转 done，用来看"一条记录一个事务" ----

func (f *fx) tripleItems(prefix string, n int) string {
	at := f.now().Add(-72 * time.Hour).In(cst).Truncate(time.Second)
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `{"type":"item","external_id":"%s-%d","item":{"kind":"task","title":"三步 %s %d"},"history":[{"at":%q,"event":"created","phase":"open"},{"at":%q,"event":"progress","text":"p","on":%q},{"at":%q,"event":"transition","to":"done"}]}`+"\n",
			prefix, i, prefix, i, rfc(at), rfc(at.Add(time.Minute)), at.Format("2006-01-02"), rfc(at.Add(2*time.Minute)))
	}
	return b.String()
}
