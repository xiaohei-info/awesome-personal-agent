//go:build e2e

package upgrade

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// fixtureModelAt 是夹具策略里 model_endpoint 的丢弃端口地址；演练里改为 MODEL_STUB 的地址（见 ACT-UPG-08）。
const (
	fixtureModelAt = "http://127.0.0.1:9/v1"
	fixtureAPIKey  = "sk-fixture-synthetic-not-a-key"
)

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

// frozenVersions 是 ACT-UPG-18 的夹具集合：每个已冻结的账本结构版本一份（LEDGER_FIXTURE_V<n>）。
func frozenVersions() []int {
	var vs []int
	for v := range ledgerfixture.Frozen {
		vs = append(vs, v)
	}
	sort.Ints(vs)
	return vs
}

// oldBinary 是回退用的旧版本主程序：清单 `binary` 字段给出环境变量名；版本 2 缺省由 fetch-v01.sh 从冻结提交构建。
func oldBinary(l *lab, m *ledgerfixture.Manifest, v int) string {
	l.t.Helper()
	if b := os.Getenv(m.Binary); b != "" {
		return b
	}
	if v == 2 {
		return l.V01Bin()
	}
	l.t.Fatalf("结构版本 %d 的回退二进制需要环境变量 %s", v, m.Binary)
	return ""
}

// TestUpgradeChain 是 flow-upgrade-rollback 的全流程：对每个已冻结版本的夹具从头走一遍（ACT-UPG-18），
// 其中包含 ACT-UPG-01..17 与 ACT-UPG-19，各动作按验收文档的顺序执行（09 的等待放在最后一次运行中的操作之后）。
func TestUpgradeChain(t *testing.T) {
	vs := frozenVersions()
	if len(vs) == 0 {
		t.Fatal("没有已冻结的账本结构版本")
	}
	latest, err := ledgerfixture.Latest()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		t.Run(fmt.Sprintf("V%d", v), func(t *testing.T) { chain(t, v, latest) })
	}
}

type run struct {
	*lab
	v, latest int
	m         *ledgerfixture.Manifest
	home      string
	obs, del  string
	bin       string // 旧版本二进制
	stub      *modelstub.Stub
	pre       map[string]string // ACT-UPG-01 的文件清单
	policyDoc map[string]string // 改指向 MODEL_STUB 之后、升级之前的策略版本文档哈希
	groups    map[string]int    // 升级前来源事件按 来源/处置/原因 的计数
	srv       *server
	session   string
	itemSeq   int
}

func chain(t *testing.T, v, latest int) {
	r := &run{lab: newLab(t), v: v, latest: latest}
	r.stub = modelstub.NewAt("127.0.0.1:0")
	// 升级演练不验模型的回答，只验请求有没有发出：替身一律答 503，待判断的跟进与决策卡片起草保持待办，不被消耗。
	r.stub.SetDefault(modelstub.Reply{Status: 503})
	t.Cleanup(r.stub.Close)

	r.step(t, "ACT-UPG-01 准备夹具副本并记录基线", r.upg01)
	r.step(t, "ACT-UPG-02 升级预检只读", r.upg02)
	r.step(t, "ACT-UPG-03 监听非本机地址缺 public_url", r.upg03)
	r.step(t, "ACT-UPG-15 迁移中途失败整体回滚", r.upg15)
	r.step(t, "ACT-UPG-04 启动时先留快照再迁移", r.upg04)
	r.step(t, "ACT-UPG-05 各表行数不减", r.upg05)
	r.step(t, "ACT-UPG-06 不改写历史 Event", r.upg06)
	r.step(t, "ACT-UPG-07 旧事项与旧判断可回放", r.upg07)
	r.step(t, "ACT-UPG-08 旧端点配置可用、策略版本不变", r.upg08)
	r.step(t, "ACT-UPG-10 默认值变更逐条留痕", r.upg10)
	r.step(t, "ACT-UPG-11 被拒的用户包有去处", r.upg11)
	r.step(t, "ACT-UPG-12 口令可登录、旧会话失效", r.upg12)
	r.step(t, "ACT-UPG-13 只升内核不升插件", r.upg13)
	r.step(t, "ACT-UPG-09 升级本身不触发重判", r.upg09)
	r.step(t, "ACT-UPG-16 回退前用新版本备份并校验", r.upg16)
	r.step(t, "ACT-UPG-14 版本闸", r.upg14)
	r.step(t, "ACT-UPG-17 旧二进制加升级前快照回退", r.upg17)
	r.step(t, "ACT-UPG-19 已冻结版本的迁移与冻结时一致", r.upg19)
}

// step 把当前步骤的 t 交给 lab（致命错误只终止本步骤），长期进程仍挂在整个测试上。
func (r *run) step(t *testing.T, name string, fn func(*testing.T)) {
	t.Helper()
	step(t, name, func(st *testing.T) { r.lab = &lab{r.Env, st, r.top}; fn(st) })
}

// extra 是带凭据访问本次服务的环境。
func (r *run) extra(tok string, more ...string) []string {
	return append([]string{"APA_URL=" + r.srv.URL, "APA_TOKEN=" + tok}, more...)
}

// direct 是"与本人私聊"的会话环境（带本人原话的命令需要）。
func (r *run) direct(tok string) []string {
	return r.extra(tok, "APA_SESSION_KIND=direct", "APA_SESSION=e2e-upgrade")
}

// copyFixture 取一份新的夹具副本（监听已改为本机空闲端口）。
func (r *run) copyFixture() string {
	fx := r.LedgerFixture(r.v)
	setListen(r.t, fx.Home, "127.0.0.1:"+freePort())
	return fx.Home
}

func (r *run) backups(home string) []string {
	ents, _ := os.ReadDir(filepath.Join(home, "backups"))
	var out []string
	for _, e := range ents {
		out = append(out, filepath.Join(home, "backups", e.Name()))
	}
	return out
}

// backlogGroups 是来源事件按 来源|处置|原因 的计数（原因为空记为 none）。
func backlogGroups(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT source_id, disposition, unjudged_reason, COUNT(*) FROM sources_runner_event GROUP BY 1, 2, 3`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var src, disp, reason string
		var n int
		if err := rows.Scan(&src, &disp, &reason, &n); err != nil {
			t.Fatal(err)
		}
		if reason == "" {
			reason = "none"
		}
		out[src+"|"+disp+"|"+reason] = n
	}
	return out
}

func (r *run) upg01(t *testing.T) {
	fx := r.LedgerFixture(r.v)
	r.m, r.home = fx.M, fx.Home
	m := r.m
	if m.SchemaLedger != r.v || m.EventMaxSeq == 0 || len(m.Tables) == 0 || len(m.EventPayloadSHA) != int(m.EventMaxSeq) ||
		m.Backlog.Total == 0 || len(m.PolicyVersions) == 0 || m.Tokens["observer"] == "" || m.Tokens["delegate"] == "" ||
		m.WebSecret == "" || m.SessionCookie == "" || len(m.Sources) == 0 || m.UserPack.Name == "" ||
		len(m.MigrationChecksums) == 0 || m.FrozenCommit == "" {
		t.Fatalf("夹具清单字段不全: schema=%d max=%d tables=%d", m.SchemaLedger, m.EventMaxSeq, len(m.Tables))
	}
	r.obs, r.del = m.Tokens["observer"], m.Tokens["delegate"]
	r.bin = oldBinary(r.lab, m, r.v)

	// 夹具里 model_endpoint 的 url 是丢弃端口；演练把它指向 MODEL_STUB（改的是副本里还没升级的策略版本，
	// 记下改后的哈希作为"逐字节不变"的基线）。policy.changed 事件的载荷不动，清单的事件哈希因此仍然成立。
	db := openRW(t, r.home)
	if _, err := db.Exec(`UPDATE policy_version SET doc = replace(doc, ?, ?)`, fixtureModelAt, r.stub.URL()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ro := openRO(t, r.home)
	r.policyDoc = map[string]string{}
	rows, err := ro.Query(`SELECT version, doc FROM policy_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ver int
		var doc string
		if err := rows.Scan(&ver, &doc); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(doc, r.stub.URL()) && strings.Contains(doc, "model_endpoint") && strings.Contains(doc, `"url"`) {
			t.Fatalf("策略版本 %d 没有指向 MODEL_STUB", ver)
		}
		r.policyDoc[itoa(ver)] = sha(doc)
	}
	rows.Close()
	if len(r.policyDoc) != len(m.PolicyVersions) {
		t.Fatalf("策略版本数 %d != 清单 %d", len(r.policyDoc), len(m.PolicyVersions))
	}
	r.groups = backlogGroups(t, ro)
	ro.Close()

	setListen(t, r.home, "127.0.0.1:"+freePort())
	r.pre = tree(t, r.home)
	if len(r.pre) == 0 {
		t.Fatal("文件清单为空")
	}
}

// upgradeReport 运行 `apa doctor --upgrade-check --json` 并解析。
type check struct {
	Name, Status, Why, Next string
	Data                    map[string]any
	Details                 []struct{ Subject, Status, Why, Next string }
}

func (r *run) doctorReport(home string, args ...string) map[string]check {
	r.t.Helper()
	return r.lab.doctorAt(home, nil, args...)
}

func (l *lab) doctorAt(home string, extra []string, args ...string) map[string]check {
	l.t.Helper()
	res := l.run(l.apaBin(), home, extra, append([]string{"doctor"}, args...)...)
	var rep struct {
		OK     bool
		Checks []check
	}
	if err := json.Unmarshal([]byte(res.Stdout), &rep); err != nil {
		l.t.Fatalf("doctor %v 输出不是报告 JSON（退出码 %d）: %v\n%s%s", args, res.Code, err, res.Stdout, res.Stderr)
	}
	out := map[string]check{}
	for _, c := range rep.Checks {
		out[c.Name] = c
	}
	return out
}

func (r *run) upg02(t *testing.T) {
	home := r.copyFixture()
	before := tree(t, home)
	rep := r.doctorReport(home, "--upgrade-check", "--json")
	sameTree(t, "apa doctor --upgrade-check", before, tree(t, home))

	sc := rep["schema_version"]
	if int(sc.Data["current"].(float64)) != r.v || int(sc.Data["target"].(float64)) != r.latest {
		t.Errorf("数据结构版本差异 = %v，期望 %d → %d", sc.Data, r.v, r.latest)
	}
	pc := rep["pack_compat"]
	found := false
	for _, d := range pc.Details {
		if d.Subject == r.m.UserPack.Name {
			found = true
			contains(t, "会被拒绝的用户能力包", d.Why, "confidence_threshold")
			contains(t, "会被拒绝的用户能力包的改法", d.Next, "appendix C")
		}
	}
	if !found {
		t.Errorf("预检没有逐字段列出会被拒绝的用户包 %s: %+v", r.m.UserPack.Name, pc)
	}
	oc := rep["source_outbound"]
	var subjects []string
	for _, d := range oc.Details {
		subjects = append(subjects, d.Subject)
	}
	want := append([]string(nil), r.m.Sources...)
	sort.Strings(subjects)
	sort.Strings(want)
	if strings.Join(subjects, ",") != strings.Join(want, ",") {
		t.Errorf("未选外发范围的来源 = %v，期望夹具全部来源 %v", subjects, want)
	}
	after := rep["after_upgrade"]
	if len(after.Details) == 0 {
		t.Errorf("预检没有列出升级后需要本人处理的事项: %+v", after)
	}
	var names []string
	for _, d := range after.Details {
		names = append(names, d.Subject)
	}
	for _, n := range []string{"plugins", "web-login", "rule-modes"} {
		if !strings.Contains(strings.Join(names, ","), n) {
			t.Errorf("升级后事项缺 %s: %v", n, names)
		}
	}
}

func (r *run) upg03(t *testing.T) {
	home := r.copyFixture()
	setListen(t, home, "0.0.0.0:"+freePort())
	before := tree(t, home)
	rep := r.doctorReport(home, "--upgrade-check", "--json")
	sameTree(t, "监听非本机地址时的 apa doctor --upgrade-check", before, tree(t, home))
	pu := rep["public_url"]
	if pu.Status != "error" && pu.Status != "warning" {
		t.Errorf("public_url 一项应为错误或提醒，实际 %q: %+v", pu.Status, pu)
	}
	contains(t, "public_url 结论", pu.Why+" "+pu.Next, "public_url", "https")
}

func (r *run) upg15(t *testing.T) {
	home := r.copyFixture()
	db := filepath.Join(home, "ledger.db")
	ledgerSHA := fileSHA(t, db)
	before := without(tree(t, home), "backups")

	s, res := r.tryServeAsIs(r.apaBin(), home, "APA_TEST_MIGRATION_FAIL_AFTER=1")
	if s != nil {
		t.Fatalf("注入失败后待测版本不应启动: %s", s.Log())
	}
	if res.Code == 0 {
		t.Fatalf("注入失败后待测版本应以非 0 退出，输出: %s", res.Stdout)
	}
	contains(t, "迁移失败时的输出", res.Stdout, "migration fault injected after step 1", "rolled back")

	if got := fileSHA(t, db); got != ledgerSHA {
		t.Errorf("迁移失败后 ledger.db 被改动（%s → %s）", ledgerSHA, got)
	}
	sameTree(t, "迁移失败后（backups/ 除外）", before, without(tree(t, home), "backups"))
	if len(r.backups(home)) != 1 {
		t.Errorf("迁移失败前应已留下一份升级前快照: %v", r.backups(home))
	}
	// 对照：同一份副本不带注入开关能正常升级，证明上面的失败来自注入而不是别的原因。
	if ok := func() bool {
		s2, res2 := r.tryServeAsIs(r.apaBin(), home)
		if s2 == nil {
			t.Errorf("不带注入开关应能升级成功: %d %s", res2.Code, res2.Stdout)
			return false
		}
		s2.stop()
		return true
	}(); !ok {
		return
	}

	// 旧版本二进制能打开失败后的账本：用第二份副本做同样的失败，再用旧二进制启动并列出待办。
	home2 := r.copyFixture()
	if s, _ := r.tryServeAsIs(r.apaBin(), home2, "APA_TEST_MIGRATION_FAIL_AFTER=1"); s != nil {
		t.Fatal("第二份副本注入失败后不应启动")
	}
	old := r.serve(r.bin, home2)
	res = r.run(r.bin, home2, []string{"APA_URL=" + old.URL, "APA_TOKEN=" + r.obs}, "inbox")
	if res.Code != 0 || !strings.Contains(res.Stdout, `"suggestions"`) {
		t.Errorf("v0.1 二进制应能正常打开并列出待办: %d %s%s", res.Code, res.Stdout, res.Stderr)
	}
}

func (r *run) upg04(t *testing.T) {
	r.srv = r.serve(r.apaBin(), r.home)
	snaps := r.backups(r.home)
	if len(snaps) != 1 || !strings.Contains(filepath.Base(snaps[0]), "pre-upgrade") {
		t.Fatalf("backups/ 下应恰有一份迁移前快照: %v", snaps)
	}
	res := r.apa(r.home, nil, 0, "backup", "verify", snaps[0])
	v := decode[struct {
		OK           bool `json:"ok"`
		SchemaLedger int  `json:"schema_ledger"`
	}](t, res.Stdout)
	if !v.OK || v.SchemaLedger != r.v {
		t.Errorf("快照校验 = %+v，期望 ok 且账本结构版本 %d", v, r.v)
	}
	ver := decode[struct {
		Schema struct{ Ledger int }
	}](t, r.apa(r.home, r.extra(r.obs), 0, "version").Stdout)
	if ver.Schema.Ledger != r.latest {
		t.Errorf("apa version 的 schema.ledger = %d，期望 %d", ver.Schema.Ledger, r.latest)
	}
	hv := decode[struct {
		Schema struct{ Ledger int }
	}](t, r.srv.get(r.obs, "/v1/version").Body)
	if hv.Schema.Ledger != r.latest {
		t.Errorf("/v1/version 的 schema.ledger = %d", hv.Schema.Ledger)
	}
}

func (r *run) upg05(t *testing.T) {
	db := openRO(t, r.home)
	counts := tableCounts(t, db)
	for tbl, n := range r.m.Tables {
		got, ok := counts[tbl]
		switch {
		case !ok:
			t.Errorf("清单里的表 %s 不存在", tbl)
		case got >= n:
		case tbl == "followup_pending":
			// 待办队列：到期的跟进被内核处理后删除该行（夹具的到期时间在过去，启动即处理）。迁移不丢数据的判据是：
			// 每个消失的行，对应事项在升级后都有新的 Event（被处理过），而不是凭空消失。
			left := map[string]bool{}
			lr, err := db.Query(`SELECT item FROM followup_pending`)
			if err != nil {
				t.Fatal(err)
			}
			for lr.Next() {
				var id string
				_ = lr.Scan(&id)
				left[id] = true
			}
			lr.Close()
			for _, id := range r.m.WaitingItems {
				if left[id] {
					continue
				}
				if n := len(events(t, db, `WHERE seq > ? AND object = ?`, r.m.EventMaxSeq, id)); n == 0 {
					t.Errorf("待办 %s 的跟进行消失了，升级后却没有该事项的任何新 Event", id)
				}
			}
			t.Logf("followup_pending %d → %d（到期跟进已被内核处理）", n, got)
		default:
			t.Errorf("表 %s 行数减少：%d → %d", tbl, n, got)
		}
	}
}

// apiEvents 用观察者凭据按游标取全部可见 Event 信封，直到追上 maxSeq。
func (r *run) apiEvents(srv *server, tok string, upTo int64) []int64 {
	r.t.Helper()
	var seqs []int64
	cursor := int64(0)
	for i := 0; i < 400; i++ {
		resp := srv.get(tok, fmt.Sprintf("/v1/events?cursor=%d&limit=500", cursor))
		if resp.Code != 200 {
			r.t.Fatalf("/v1/events: %d %s", resp.Code, resp.Body)
		}
		out := decode[struct {
			Events []struct{ Seq int64 }
			Next   int64
		}](r.t, resp.Body)
		for _, e := range out.Events {
			seqs = append(seqs, e.Seq)
		}
		if len(out.Events) == 0 || out.Next >= upTo || out.Next <= cursor {
			break
		}
		cursor = out.Next
	}
	return seqs
}

func (r *run) upg06(t *testing.T) {
	db := openRO(t, r.home)
	all := events(t, db, "")
	max := r.m.EventMaxSeq
	for i, e := range all {
		if e.Seq != int64(i+1) {
			t.Fatalf("Event 序号不连续：第 %d 条的序号是 %d", i+1, e.Seq)
		}
		if e.Seq <= max {
			if want := r.m.EventPayloadSHA[itoa(int(e.Seq))]; e.PayloadHashLower != want {
				t.Errorf("升级前的 Event %d 载荷哈希变了：%s → %s", e.Seq, want, e.PayloadHashLower)
			}
		}
	}
	if int64(len(all)) <= max {
		t.Fatalf("升级应在末尾追加 Event（如 rule.mode_changed）：共 %d 条，升级前 %d", len(all), max)
	}
	// 对外接口看到的也一致：API 序号严格递增，都是账本里的序号；升级产生的都在旧序号之后。
	seqs := r.apiEvents(r.srv, r.obs, int64(len(all)))
	if len(seqs) == 0 {
		t.Fatal("API 没有返回任何 Event")
	}
	for i, s := range seqs {
		if s < 1 || s > int64(len(all)) || (i > 0 && s <= seqs[i-1]) {
			t.Fatalf("API 序号异常: 第 %d 个是 %d", i, s)
		}
	}
	if seqs[len(seqs)-1] <= max {
		t.Errorf("API 末尾序号 %d 应大于升级前最大序号 %d", seqs[len(seqs)-1], max)
	}
}

func (r *run) upg07(t *testing.T) {
	db := openRO(t, r.home)
	ids := append([]string(nil), r.m.WaitingItems[:3]...)
	judged := one[string](t, db, `SELECT object FROM eventlog_event WHERE type = 'run.finished' AND seq <= ? AND payload LIKE '%"category":"judge"%' ORDER BY seq LIMIT 1`, r.m.EventMaxSeq)
	ids = append(ids, judged)
	for _, id := range ids {
		res := r.apa(r.home, r.extra(r.obs), 0, "why", id)
		why := decode[struct {
			ID    string
			Steps []struct {
				Seq  int64
				Type string
			}
		}](t, res.Stdout)
		if why.ID != id || len(why.Steps) == 0 {
			t.Errorf("apa why %s 没有时间线: %s", id, res.Stdout)
		}
		if id == judged {
			has := false
			for _, s := range why.Steps {
				has = has || s.Type == "run.finished"
			}
			if !has {
				t.Errorf("旧判断应以 v0.1 的 run.finished 显示: %s", res.Stdout)
			}
		} else {
			contains(t, "旧事项 "+id+" 的时间线", res.Stdout, "item.created")
		}
	}
}

// newDecision 用委托凭据新建事项并提出决策，返回事项 id。
func (r *run) newDecision(title string) string {
	r.t.Helper()
	r.itemSeq++
	res := r.apa(r.home, r.direct(r.del), 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--evidence", "升级演练造一条事项")
	id := decode[struct{ ID string }](r.t, res.Stdout).ID
	r.apa(r.home, r.direct(r.del), 0, "ask", id, "--question", "选哪个", "--option", "A=甲", "--option", "B=乙", "--recommend", "A")
	return id
}

func (r *run) upg08(t *testing.T) {
	code, cookie, csrf := r.srv.webLogin(r.m.WebSecret)
	if code != 200 || cookie == "" {
		t.Fatalf("夹具口令登录失败: %d", code)
	}
	r.session = cookie
	web := r.srv.web(cookie, csrf, "GET", "/v1/settings/model-endpoints", "")
	if web.Code != 200 {
		t.Fatalf("读取端点设置: %d %s", web.Code, web.Body)
	}
	got := decode[struct {
		Generate struct{ URL, Locality, Model string } `json:"generate_endpoint"`
	}](t, web.Body)
	if got.Generate.URL != r.stub.URL() || got.Generate.Locality != "external" {
		t.Errorf("generate_endpoint = %+v，期望等于旧 model_endpoint（%s）且位置 external", got.Generate, r.stub.URL())
	}
	if strings.Contains(web.Body, fixtureAPIKey) {
		t.Errorf("读取端点设置不应回显密钥明文")
	}

	before := len(r.stub.Requests())
	r.newDecision("升级演练：决策卡片起草")
	waitFor(t, "旧端点配置发出生成请求", 40*time.Second, func() bool { return len(r.stub.Requests()) > before })
	req := r.stub.Requests()[before]
	if !strings.HasSuffix(req.Path, "/chat/completions") || !strings.Contains(strings.Join(req.Header["Authorization"], ""), fixtureAPIKey) {
		t.Errorf("生成请求没有用旧配置的路径与密钥: %s %v", req.Path, req.Header["Authorization"])
	}

	db := openRO(t, r.home)
	rows, err := db.Query(`SELECT version, doc FROM policy_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ver int
		var doc string
		if err := rows.Scan(&ver, &doc); err != nil {
			t.Fatal(err)
		}
		if want, ok := r.policyDoc[itoa(ver)]; ok && sha(doc) != want {
			t.Errorf("已存的策略版本 %d 被改写", ver)
		}
	}
}

func (r *run) upg09(t *testing.T) {
	// 验收文档等 15 分钟（长于后台维护循环的一轮与 10 分钟退避）。用测试时钟把"现在"拨快一天，
	// 再等过后台维护循环的一轮（一分钟一次）：两种推进叠加，等价于足够长的真实等待。
	if err := os.WriteFile(filepath.Join(r.home, "test-clock-offset"), []byte("86400\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reqBefore := len(r.stub.Requests())
	time.Sleep(75 * time.Second)

	ro := openRO(t, r.home)
	if got := backlogGroups(t, ro); fmt.Sprint(got) != fmt.Sprint(r.groups) {
		t.Errorf("积压的处置分布变了：\n升级前 %v\n现在   %v", r.groups, got)
	}
	// 对外接口：逐来源取列表，与清单 backlog 比较。
	byDisp := map[string]int{}
	for _, src := range r.m.Sources {
		resp := r.srv.get(r.obs, "/v1/sources/"+src+"/events?disposition=unjudged,expired,ignored,proposed,attached&limit=500")
		if resp.Code != 200 {
			t.Fatalf("来源 %s 的事件列表: %d %s", src, resp.Code, resp.Body)
		}
		for _, e := range decode[struct {
			Events []struct {
				Disposition    string
				UnjudgedReason string `json:"unjudged_reason"`
			}
		}](t, resp.Body).Events {
			reason := e.UnjudgedReason
			if reason == "" {
				reason = "none"
			}
			if e.Disposition == "unjudged" {
				byDisp["unjudged|"+reason]++
			} else {
				byDisp[e.Disposition]++
			}
		}
	}
	b := r.m.Backlog
	if byDisp["expired"] != b.Expired {
		t.Errorf("expired = %d，清单 %d", byDisp["expired"], b.Expired)
	}
	for reason, n := range b.Unjudged {
		if byDisp["unjudged|"+reason] != n {
			t.Errorf("unjudged(%s) = %d，清单 %d", reason, byDisp["unjudged|"+reason], n)
		}
	}
	// 没有重判、没有放回，被旧级联忽略的事件仍是 ignored。
	if n := len(events(t, ro, `WHERE seq > ? AND type IN ('inbound.requeued', 'inbound.disposed')`, r.m.EventMaxSeq)); n != 0 {
		t.Errorf("升级后出现了 %d 条 inbound.requeued/inbound.disposed", n)
	}
	for _, id := range append(append([]string(nil), r.m.RuleIgnored...), r.m.UserIgnored...) {
		if d := one[string](t, ro, `SELECT disposition FROM sources_runner_event WHERE id = ?`, id); d != "ignored" {
			t.Errorf("事件 %s 应保持 ignored，现为 %s", id, d)
		}
	}
	oc := one[int](t, ro, `SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'ignored' AND id IN (SELECT object FROM eventlog_event WHERE type = 'run.finished' AND seq <= ?)`, r.m.EventMaxSeq)
	if oc != r.m.OldCascadeIgnored.Count+len(r.m.OutsideWindow) {
		t.Errorf("被旧级联忽略的事件应原样保持 ignored：%d，清单 %d+%d", oc, r.m.OldCascadeIgnored.Count, len(r.m.OutsideWindow))
	}
	for _, q := range r.stub.Requests()[reqBefore:] {
		if strings.Contains(q.BodyText, "[FX-") {
			t.Errorf("替身收到了来源事件的判断题请求: %.200s", q.BodyText)
		}
	}
}

type ruleChange struct{ Rule, From, To, Why string }

func (r *run) upg10(t *testing.T) {
	db := openRO(t, r.home)
	rm := r.m.RuleModes
	var switched = map[string]ruleChange{}
	var anyEvent = map[string][]string{} // 规则 → 升级后所有 rule.mode_changed 的说明
	for _, e := range events(t, db, `WHERE seq > ? AND type = 'rule.mode_changed'`, r.m.EventMaxSeq) {
		c := decode[ruleChange](t, e.Payload)
		anyEvent[c.Rule] = append(anyEvent[c.Rule], c.From+"→"+c.To+" ("+c.Why+", "+e.ActorPrincipal+")")
		if e.ActorType == "system" && e.ActorPrincipal == "system" {
			if c.From != "observe" || c.To != "auto" {
				t.Errorf("升级切换应是 observe → auto: %+v", c)
			}
			switched[c.Rule] = c
		}
	}
	mode := func(key string) string { return one[string](t, db, `SELECT mode FROM rules_state WHERE key = ?`, key) }
	// 未动过的内置提醒类规则：各一条 observe → auto（操作者 system）；动作类（事件规则）不切换。
	for _, key := range rm.Untouched {
		if strings.Contains(key, "/clock_rules/") {
			if _, ok := switched[key]; !ok || mode(key) != "auto" {
				t.Errorf("未动过的提醒类规则 %s 应有 observe→auto 事件且模式为 auto: %v / %s", key, anyEvent[key], mode(key))
			}
		}
	}
	for key, mo := range rm.Current {
		if strings.Contains(key, "/event_rules/") && (mode(key) != mo || len(anyEvent[key]) != 0) {
			t.Errorf("动作类规则 %s 应不变（%s）也不留事件: %s %v", key, mo, mode(key), anyEvent[key])
		}
	}
	// 本人切换过的、用户包显式写 observe 的、默认关闭的规则：模式不变，没有任何升级后的模式事件。
	var keep []string
	for key := range rm.OwnerSwitched {
		keep = append(keep, key)
	}
	keep = append(keep, rm.UserPackExplicitObserve...)
	keep = append(keep, rm.OffByDefault...)
	for _, key := range keep {
		if mode(key) != rm.Current[key] || len(anyEvent[key]) != 0 {
			t.Errorf("规则 %s 应保持 %s 且没有模式事件，实际 %s %v", key, rm.Current[key], mode(key), anyEvent[key])
		}
	}
	h := decode[struct {
		Signals []struct {
			ID     string
			Detail json.RawMessage
		}
	}](t, r.run(r.apaBin(), r.home, r.extra(r.obs), "health").Stdout)
	merged := false
	for _, s := range h.Signals {
		merged = merged || (s.ID == "other_alerts" && strings.Contains(string(s.Detail), "rules:upgrade"))
	}
	// 夹具里没有“未动过的内置提醒类规则”可切（follow-up 的用户包被拒、规则不自动切换，GT-345），升级后不应有任何切换，
	// 也就不应有合并说明；一旦有切换，就必须有这条说明。observe→auto 的切换路径由 kernel/rules 的单元测试覆盖。
	if len(switched) > 0 && !merged {
		t.Errorf("有 %d 条规则被切到 auto，健康视图应有一条合并后的规则模式说明（rules:upgrade）", len(switched))
	}
	if len(switched) == 0 && merged {
		t.Errorf("没有规则被切换却出现了 rules:upgrade 说明")
	}
}

func (r *run) upg11(t *testing.T) {
	res := r.run(r.apaBin(), r.home, r.extra(r.obs), "health")
	h := decode[struct {
		Signals []struct {
			ID         string
			OK         bool
			Rejections []struct{ Pack, Source, Reason string }
		}
	}](t, res.Stdout)
	got := false
	for _, s := range h.Signals {
		if s.ID != "pack_rejections" {
			continue
		}
		for _, x := range s.Rejections {
			if x.Pack == r.m.UserPack.Name {
				got = true
				contains(t, "健康视图 pack_rejections", x.Reason, "confidence_threshold", "band", "answer_by")
			}
		}
	}
	if !got {
		t.Errorf("健康视图 pack_rejections 应逐字段列出被拒的 %s: %s", r.m.UserPack.Name, res.Stdout)
	}
	rep := r.doctorReport(r.home, "--json")
	found := false
	for _, d := range rep["packs"].Details {
		if d.Subject == r.m.UserPack.Name {
			found = true
			contains(t, "apa doctor 的能力包一项", d.Why, "confidence_threshold", "answer_by")
		}
	}
	if !found {
		t.Errorf("apa doctor 的能力包一项应列出 %s: %+v", r.m.UserPack.Name, rep["packs"])
	}
	// Web 设置页读到的加载结果：同名内置包生效，用户包不在生效列表里。
	code, cookie, csrf := r.srv.webLogin(r.m.WebSecret)
	if code != 200 {
		t.Fatalf("登录: %d", code)
	}
	pk := r.srv.web(cookie, csrf, "GET", "/v1/packs", "")
	if pk.Code != 200 {
		t.Fatalf("GET /v1/packs: %d %s", pk.Code, pk.Body)
	}
	var packs struct {
		Packs []struct {
			Name, Source string
			Builtin      bool
		}
	}
	_ = json.Unmarshal([]byte(pk.Body), &packs)
	effective := 0
	for _, p := range packs.Packs {
		if p.Name == r.m.UserPack.Name {
			effective++
			if p.Source == "user" {
				t.Errorf("被拒的用户包不应生效: %+v", p)
			}
		}
	}
	if effective != 1 {
		t.Errorf("同名内置包应恰好生效一份，实际 %d: %s", effective, pk.Body)
	}
}

func (r *run) upg12(t *testing.T) {
	if resp := httpDo(t, "GET", r.srv.URL+"/v1/items", "", map[string]string{"Cookie": r.m.SessionCookie}); resp.Code != 401 {
		t.Errorf("升级前的登录会话应失效（401），实际 %d", resp.Code)
	}
	if len(r.m.WebSecret) >= 12 {
		t.Errorf("夹具口令应短于新版本的 12 个字符下限才能证明旧口令可用：%d", len(r.m.WebSecret))
	}
	if code, _, _ := r.srv.webLogin(r.m.WebSecret); code != 200 {
		t.Errorf("已有口令应能登录，实际 %d", code)
	}
}

func (r *run) upg13(t *testing.T) {
	// 一份"旧插件"的声明：自检命令报告它不上报会话类型。
	script := filepath.Join(t.TempDir(), "selfcheck.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo '{"plugin_version":"0.1.0","hooks":[],"reports_session_kind":false,"overlaps":[]}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := r.Bin["stub-harness"]
	decl, _ := json.Marshal(map[string]any{"harness": "acc-oldplug",
		"headless": map[string]any{"command": []string{bin, "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
			"access": map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}}},
		"selfcheck": map[string]any{"command": []string{script}}})
	declFile := filepath.Join(t.TempDir(), "decl.json")
	if err := os.WriteFile(declFile, decl, 0o644); err != nil {
		t.Fatal(err)
	}
	r.apa(r.home, r.direct(r.del), 0, "declare", declFile, "--evidence", "模拟未升级的旧插件")
	code, cookie, csrf := r.srv.webLogin(r.m.WebSecret)
	if code != 200 {
		t.Fatalf("登录: %d", code)
	}
	if c := r.srv.web(cookie, csrf, "POST", "/v1/settings/declarations/acc-oldplug/1/confirm", "").Code; c != 200 {
		t.Fatalf("确认声明: %d", c)
	}

	id := r.newDecision("升级演练：旧插件代答")
	db := openRO(t, r.home)
	// 已委派的事项与它的执行者凭据：回执（apa report）是旧插件仍然会用的写法。
	prj := one[string](t, db, `SELECT id FROM projects ORDER BY id LIMIT 1`)
	r.mustWeb(cookie, csrf, "POST", "/v1/settings/executors", `{"id":"upg-host.cli","kind":"external-cli"}`, 200)
	var wrk struct{ Secret string }
	if err := json.Unmarshal([]byte(r.mustWeb(cookie, csrf, "POST", "/v1/settings/tokens", `{"kind":"worker","label":"WRK","executor":"upg-host.cli"}`, 201)), &wrk); err != nil || wrk.Secret == "" {
		t.Fatalf("签发 worker 凭据: %v", err)
	}
	r.mustWeb(cookie, csrf, "PUT", "/v1/settings/projects/"+prj+"/allowed-executors", `{"executors":["upg-host.cli"]}`, 200)
	delegated := decode[struct{ ID string }](t, r.apa(r.home, r.direct(r.del), 0, "item", "add", "--kind", "task", "--title", "升级演练：已委派事项",
		"--phase", "open", "--assignee", "upg-host.cli", "--project", prj, "--evidence", "造一条已委派事项").Stdout).ID

	old := r.extra(r.del, "APA_SESSION=e2e-upgrade") // 不上报会话类型（没有 APA_SESSION_KIND）
	res := r.run(r.apaBin(), r.home, old, "decide", id, "--option", "A", "--evidence", "按本人原话同意")
	if res.Code != 23 || !strings.Contains(res.Stderr+res.Stdout, "session_not_direct") {
		t.Errorf("凭原话的代答应被拒（退出码 23 / session_not_direct）: %d %s%s", res.Code, res.Stdout, res.Stderr)
	}
	show := r.apa(r.home, r.extra(r.obs), 0, "item", "show", id)
	contains(t, "被拒的代答不应生效", show.Stdout, `"phase":"needs_input"`)

	if res := r.run(r.apaBin(), r.home, old, "context", "--scope", "all"); res.Code != 0 {
		t.Errorf("apa context 读取应不受影响: %d %s%s", res.Code, res.Stdout, res.Stderr)
	}
	rcpt := r.extra(wrk.Secret) // 同样不上报会话类型
	if res := r.run(r.apaBin(), r.home, rcpt, "report", delegated, "--outcome", "progress", "--summary", "旧回执写法的进展"); res.Code != 0 {
		t.Errorf("旧回执写法的 apa report 应不受影响: %d %s%s", res.Code, res.Stdout, res.Stderr)
	}
	rep := r.doctorReport(r.home, "--json")
	found := false
	for _, d := range rep["host_plugin"].Details {
		if d.Subject == "acc-oldplug" {
			found = true
			contains(t, "doctor 宿主插件一项", d.Why+" "+d.Next, "does not report the session type", "upgrade the plugin together with the kernel")
		}
	}
	if !found {
		t.Errorf("doctor 宿主插件一项应指出旧插件: %+v", rep["host_plugin"])
	}
}

func (r *run) upg16(t *testing.T) {
	out := filepath.Join(r.Root, "newbk")
	res := r.apa(r.home, nil, 0, "backup", "create", "--out", out)
	v := decode[struct {
		OK           bool `json:"ok"`
		SchemaLedger int  `json:"schema_ledger"`
	}](t, res.Stdout)
	if !v.OK || v.SchemaLedger != r.latest {
		t.Errorf("备份结果 = %+v，期望账本结构版本 %d", v, r.latest)
	}
	r.apa(r.home, nil, 0, "backup", "verify", out)
	mf := decode[struct{ Schema struct{ Ledger int } }](t, string(mustRead(t, filepath.Join(out, "manifest.json"))))
	if mf.Schema.Ledger != r.latest {
		t.Errorf("备份清单里的账本结构版本 = %d，期望 %d", mf.Schema.Ledger, r.latest)
	}
}

func (r *run) mustWeb(cookie, csrf, method, path, body string, want int) string {
	r.t.Helper()
	x := r.srv.web(cookie, csrf, method, path, body)
	if x.Code != want {
		r.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, x.Code, want, x.Body)
	}
	return x.Body
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *run) upg14(t *testing.T) {
	r.srv.stop()
	db := filepath.Join(r.home, "ledger.db")
	before := fileSHA(t, db)
	s, res := r.tryServeAsIs(r.bin, r.home)
	if s != nil {
		t.Fatalf("v0.1 二进制不应带着新结构账本启动: %s", s.Log())
	}
	// 输出文本由冻结提交的 v0.1 二进制决定（同样只有机制性说明），不能要求它说明"结构版本高于本程序"；断言退出码与账本未被改动。
	if res.Code == 0 {
		t.Errorf("v0.1 二进制应以非 0 退出: %s", res.Stdout)
	}
	if after := fileSHA(t, db); after != before {
		t.Errorf("v0.1 二进制拒绝启动时不应改动 ledger.db")
	}
}

func (r *run) upg17(t *testing.T) {
	snaps := r.backups(r.home)
	var snap string
	for _, s := range snaps {
		if strings.Contains(filepath.Base(s), "pre-upgrade") {
			snap = s
		}
	}
	if snap == "" {
		t.Fatalf("没有升级前快照: %v", snaps)
	}
	r.apa(r.home, nil, 0, "backup", "verify", snap)
	rb := filepath.Join(r.Root, "rb")
	r.apa(r.home, nil, 0, "backup", "restore", snap, "--home", rb)
	if got := tableCounts(t, openRO(t, rb)); fmt.Sprint(got) != fmt.Sprint(r.m.Tables) {
		t.Errorf("恢复后各表行数与清单不一致:\n清单 %v\n恢复 %v", r.m.Tables, got)
	}
	// 恢复出来的账本与升级前一致：行数见上，最大 Event 序号与每条载荷哈希也相同。
	rdb := openRO(t, rb)
	if max := one[int64](t, rdb, `SELECT MAX(seq) FROM eventlog_event`); max != r.m.EventMaxSeq {
		t.Errorf("回退后账本最大 Event 序号 = %d，期望 %d", max, r.m.EventMaxSeq)
	}
	// v0.1 二进制在恢复出的目录上正常启动（观察者 token 读得到待办与事件流）。
	old := r.serve(r.bin, rb)
	if res := r.run(r.bin, rb, []string{"APA_URL=" + old.URL, "APA_TOKEN=" + r.obs}, "inbox"); res.Code != 0 {
		t.Errorf("v0.1 二进制应能列出待办: %d %s%s", res.Code, res.Stdout, res.Stderr)
	}
	seqs := r.apiEvents(old, r.obs, r.m.EventMaxSeq)
	if len(seqs) == 0 || seqs[0] != 1 {
		t.Errorf("v0.1 事件流应从序号 1 起可读: %v", seqs)
	}
	// 升级前的每条 Event 载荷不变；v0.1 启动后自己写的只有它的规则对到期跟进的观察记录（rule.observed），
	// 不是回退带来的数据变化。
	for _, e := range events(t, rdb, "") {
		if e.Seq <= r.m.EventMaxSeq {
			if e.PayloadHashLower != r.m.EventPayloadSHA[itoa(int(e.Seq))] {
				t.Errorf("回退后 Event %d 的载荷变了", e.Seq)
			}
		} else if e.Type != "rule.observed" {
			t.Errorf("v0.1 启动后新增的 Event %d 是 %s，只应有它自己的 rule.observed", e.Seq, e.Type)
		}
	}
	old.stop()

	// 用户文档：回退只有"恢复快照"一条路，不提供逆向迁移脚本。
	doc := string(mustRead(t, filepath.Join(repoRoot(), "docs", "user", "upgrade.md")))
	i := strings.Index(doc, "## 回退")
	if i < 0 {
		t.Fatal("用户文档没有回退一节")
	}
	sec := doc[i:]
	if j := strings.Index(sec[3:], "\n## "); j >= 0 {
		sec = sec[:j+3]
	}
	contains(t, "用户文档的回退一节", sec, "恢复快照", "apa backup restore", "apa backup create")
	for _, bad := range []string{"downgrade", "migrate down", "逆向迁移脚本", "反向迁移脚本"} {
		if strings.Contains(sec, bad) {
			t.Errorf("回退一节不应要求 %q", bad)
		}
	}
}

func (r *run) upg19(t *testing.T) {
	// 验收文档用 `git show <冻结提交>:<迁移文件>` 对整文件做 diff；整文件会随后续版本的迁移追加而变，
	// 夹具 README 已说明该做法不成立。等价且可核对的冻结证据是每一步迁移的校验和：
	// 升级后的账本里，冻结版本含有的每一步迁移都以冻结时的校验和登记着，且升级前的登记原样保留。
	db := openRO(t, r.home)
	rows, err := db.Query(`SELECT module || '/' || version || '/' || checksum FROM store_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		have[s] = true
	}
	rows.Close()
	for _, c := range r.m.MigrationChecksums {
		if !have[c] {
			t.Errorf("冻结时的迁移 %s 在升级后的账本里没有（被修改、合并或删除）", c)
		}
	}
	if len(have) <= len(r.m.MigrationChecksums) {
		t.Errorf("升级应只追加迁移：升级后 %d 步，冻结版本 %d 步", len(have), len(r.m.MigrationChecksums))
	}
}
