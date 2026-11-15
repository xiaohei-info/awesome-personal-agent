package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func conflict(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeConflict
}

// declareByLabel 是按 config 取名的接入层声明（如同 source.yaml 的模板）：pull-feed 与 feed 类 webhook 声明「<label> 订阅」，
// 没有 label 时声明「订阅」；/opt/check.sh 这类没有声明的程序取不到名字。
func declareByLabel(s domain.Source) string {
	if s.Command != "pull-feed" && !(s.Type == domain.SourceWebhook && s.SourceKind == "feed") {
		return ""
	}
	var c struct{ Label string }
	_ = json.Unmarshal(s.Config, &c)
	if c.Label == "" {
		return "订阅"
	}
	return c.Label + " 订阅"
}

func invalidReq(err error) bool { return exit(err) == domain.ExitUsage }

// 显示名规则只在 plan() 里，每个登记入口都覆盖（§9.1）：显式给定 > 接入层声明（同一程序多实例按 config 取名不同）>
// 都没有就拒绝；显式与声明的名字重名都返回 409；被拒的登记不落盘。
func TestEveryRegistrationEntryAppliesTheNameRule(t *testing.T) {
	webhook := func(id string) m { return m{"id": id, "type": "webhook", "source_type": "feed"} }
	undeclared := func(id string) m { s := script(id); s["source_type"] = "feed"; return s } // /opt/check.sh：第三方程序，没有声明
	entries := map[string]func(e *env, in m) error{
		"source.register pull（Web、CLI、delegate）": func(e *env, in m) error { _, err := e.do(owner, CmdRegister, in); return err },
		"source.register webhook":                func(e *env, in m) error { _, err := e.do(owner, CmdRegister, in); return err },
		"source.register_script（只 Web）":          func(e *env, in m) error { _, err := e.do(owner, CmdRegisterExec, in); return err },
		"主机配置文件 sources.json": func(e *env, in m) error {
			path := filepath.Join(t.TempDir(), "sources.json")
			if err := os.WriteFile(path, js(m{"sources": []m{in}}), 0o600); err != nil {
				t.Fatal(err)
			}
			return LoadFile(bg, e.x, owner, path)
		},
	}
	// 每个入口有声明的登记形状：script 类型的入口用 /opt/feed.sh，下面的声明把它当作与 pull-feed 同一个程序。
	declaredShape := map[string]func(string) m{
		"source.register pull（Web、CLI、delegate）": pull, "source.register webhook": webhook,
		"source.register_script（只 Web）": scriptFeed, "主机配置文件 sources.json": scriptFeed,
	}
	for name, register := range entries {
		t.Run(name, func(t *testing.T) {
			e := setupWith(t, func(s domain.Source) string {
				if s.Command == "/opt/feed.sh" {
					s.Command = "pull-feed" // 同一个声明，script 类型的入口借它
				}
				return declareByLabel(s)
			})
			src := declaredShape[name]
			labeled := func(id, label string) m { x := src(id); x["config"] = m{"label": label}; return x }

			// 显式给定：去掉首尾空白，优先于声明。
			named := labeled("src_named", "工作")
			named["name"] = "  我的订阅  "
			if err := register(e, named); err != nil {
				t.Fatal(err)
			}
			if got := e.get(t, "src_named").Name; got != "我的订阅" {
				t.Fatalf("explicit: %q", got)
			}
			// 接入层声明：同一程序的两个实例按 config 取到不同的名字。
			for id, want := range map[string]string{"src_work": "工作 订阅", "src_home": "家里 订阅"} {
				if err := register(e, labeled(id, strings.Fields(want)[0])); err != nil {
					t.Fatal(err)
				}
				if got := e.get(t, id).Name; got != want {
					t.Fatalf("declared %s: %q, want %q", id, got, want)
				}
			}
			// 与声明出的名字重名、与本人起的名字重名：都 409，不自动加序号。
			if err := register(e, labeled("src_dup", "工作")); !conflict(err) {
				t.Fatalf("declared name taken: %v", err)
			}
			dup := labeled("src_dup", "别的")
			dup["name"] = "我的订阅"
			if err := register(e, dup); !conflict(err) {
				t.Fatalf("explicit name taken: %v", err)
			}
			// 程序没有声明、也没给名字：拒绝，并说明怎么补。
			bare := undeclared("src_bare")
			if name == "source.register pull（Web、CLI、delegate）" {
				bare = pull("src_bare")
				bare["command"] = "pull-http" // 附带的另一个拉取程序，没有声明
			}
			if name == "source.register webhook" {
				bare = m{"id": "src_bare", "type": "webhook", "source_type": "forge"}
			}
			err := register(e, bare)
			if !invalidReq(err) || !strings.Contains(err.Error(), "declares none") {
				t.Fatalf("no name and no declaration: %v", err)
			}
			for _, id := range []string{"src_dup", "src_bare"} {
				if err := e.st.View(bg, func(tx *store.Tx) error { _, err := Get(tx, id); return err }); !isNotFound(err) {
					t.Fatalf("a rejected registration must not be stored: %s %v", id, err)
				}
			}
		})
	}
}

func scriptFeed(id string) m {
	s := script(id)
	s["source_type"], s["command"] = "feed", "/opt/feed.sh"
	return s
}

func TestNameValidation(t *testing.T) {
	e := setup(t)
	long := pull("src_long")
	long["name"] = strings.Repeat("长", MaxName+1)
	if _, err := e.do(owner, CmdRegister, long); exit(err) != domain.ExitUsage {
		t.Fatalf("too long: %v", err)
	}
	ctl := pull("src_ctl")
	ctl["name"] = "a\nb"
	if _, err := e.do(owner, CmdRegister, ctl); exit(err) != domain.ExitUsage {
		t.Fatalf("control character: %v", err)
	}
	ok := pull("src_max")
	ok["name"] = strings.Repeat("长", MaxName)
	if _, err := e.do(owner, CmdRegister, ok); err != nil {
		t.Fatal(err)
	}
	// 不区分大小写判重。
	up := pull("src_upper")
	up["name"] = "订阅源"
	if _, err := e.do(owner, CmdRegister, up); err != nil {
		t.Fatal(err)
	}
	up2 := pull("src_upper2")
	up2["name"] = strings.ToUpper("订阅源")
	if _, err := e.do(owner, CmdRegister, up2); !conflict(err) {
		t.Fatal("same name must conflict")
	}
	lat := pull("src_lat")
	lat["name"] = "Inbox"
	lat2 := pull("src_lat2")
	lat2["name"] = "inbox"
	if _, err := e.do(owner, CmdRegister, lat); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdRegister, lat2); !conflict(err) {
		t.Fatalf("case-insensitive duplicate: %v", err)
	}
}

func TestRenameIsRecordedIdempotentAndResettable(t *testing.T) {
	e := setupWith(t, declareByLabel)
	for id, label := range map[string]string{"src_a": "", "src_b": "家里"} {
		p := pull(id)
		p["config"] = m{"label": label}
		if _, err := e.do(owner, CmdRegister, p); err != nil {
			t.Fatal(err)
		}
	}
	before := len(e.types(t))
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": " 工作订阅 "}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_a"); s.Name != "工作订阅" {
		t.Fatalf("%+v", s)
	}
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": "工作订阅"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": "家里 订阅"}); !conflict(err) {
		t.Fatalf("rename onto another source's name: %v", err)
	}
	// 空名改回接入层声明的名字。
	if _, err := e.do(owner, CmdRename, m{"id": "src_a", "name": ""}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_a"); s.Name != "订阅" {
		t.Fatalf("reset: %+v", s)
	}
	// 没有声明的来源不能清空名字。
	sc := script("src_c")
	sc["name"] = "检查"
	if _, err := e.do(owner, CmdRegisterExec, sc); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdRename, m{"id": "src_c", "name": " "}); !invalidReq(err) || e.get(t, "src_c").Name != "检查" {
		t.Fatalf("reset without a declaration: %v", err)
	}
	if _, err := e.do(owner, CmdRename, m{"id": "src_none", "name": "x"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("unknown source: %v", err)
	}
	var renamed []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			if x.Event.Type == EvRenamed {
				renamed = append(renamed, x)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := len(e.types(t)) - before - 1; got != 2 || len(renamed) != 2 { // 减去 src_c 的登记
		t.Fatalf("two renames recorded, the no-op and the rejected one not: %d events, %d renamed", got, len(renamed))
	}
	var p map[string]string
	_ = json.Unmarshal(renamed[0].Event.Payload, &p)
	if renamed[0].Event.Object != "src_a" || p["name"] != "工作订阅" || p["previous"] != "订阅" || renamed[0].Event.Actor.Principal != "owner" {
		t.Fatalf("payload: %+v %v", renamed[0].Event, p)
	}
}

// 升级前登记的来源没有名字：Backfill 先用接入层声明（同名加序号），取不到的才按类型兜底，并为兜底的记一条健康说明；
// 其他什么都不写，再跑一次什么也不做。
func TestBackfillPrefersDeclaredNames(t *testing.T) {
	e := setupWith(t, declareByLabel)
	p := pull("src_has")
	p["name"] = "订阅"
	if _, err := e.do(owner, CmdRegister, p); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, r := range [][3]string{{"src_f1", "feed", "pull-feed"}, {"src_f2", "feed", "pull-feed"}, {"src_m1", "mail", "pull-http"}, {"src_m2", "mail", "pull-http"}, {"src_x", "other", "pull-http"}} {
			if _, err := tx.Exec(`INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path,
				dedupe_domain, config, cursor, enabled, stats) VALUES (?, 'pull', ?, ?, '1h', 30, '', '', '', 'c9', 1, '{"runs":3}')`, r[0], r[1], r[2]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := e.types(t)
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	n, err := Backfill(bg, e.st, declareByLabel, map[string]string{"mail": "邮件"}, at)
	if err != nil || n != 5 {
		t.Fatalf("%d %v", n, err)
	}
	want := map[string]string{"src_has": "订阅", "src_f1": "订阅 2", "src_f2": "订阅 3", "src_m1": "邮件", "src_m2": "邮件 2", "src_x": "定时拉取"}
	for id, w := range want {
		if s := e.get(t, id); s.Name != w || (id != "src_has" && (s.Cursor != "c9" || string(s.Stats) != `{"runs":3}`)) {
			t.Errorf("%s: %+v, want %q", id, s, w)
		}
	}
	added := e.types(t)[len(before):]
	if strings.Join(added, ",") != strings.Repeat("source.renamed,", 5)+domain.EvHealthAlert {
		t.Fatalf("five renames and one health note: %v", added)
	}
	var note eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, 0, 10)
		if len(es) == 1 {
			note = es[0]
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var np struct{ Sources []string }
	_ = json.Unmarshal(note.Event.Payload, &np)
	if note.Event.Object != NamesObject || strings.Join(np.Sources, ",") != "src_m1,src_m2,src_x" {
		t.Fatalf("health note lists only the type-named sources: %+v %v", note.Event, np)
	}
	if n, err := Backfill(bg, e.st, declareByLabel, nil, at); err != nil || n != 0 || len(e.types(t)) != len(before)+6 {
		t.Fatalf("second run: %d %v", n, err)
	}
}
