//go:build e2e

package ops

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

// sweepWait 是等内核定时清理的上限（内核每分钟清理一次）。
const sweepWait = 100 * time.Second

type ingestRec struct {
	ID          string  `json:"id"`
	DeliveryKey string  `json:"delivery_key"`
	State       string  `json:"state"`
	Signed      bool    `json:"signed"`
	Events      int     `json:"events"`
	BodySize    int     `json:"body_size"`
	Body        *string `json:"body"`
}

// postIngest 向 webhook 来源推一条数据，返回入站记录。
func (f *fx) postIngest(source, token, delivery, payload string) ingestRec {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.URL+"/ingest/"+source, bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Acc-Token", token)
	req.Header.Set("X-Acc-Event", "created")
	req.Header.Set("X-Delivery", delivery)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		f.t.Fatalf("POST /ingest/%s: %d", source, resp.StatusCode)
	}
	return f.waitIngest(source, delivery)
}

func (f *fx) ingests(source string) []ingestRec {
	f.t.Helper()
	return decode[struct {
		Ingests []ingestRec `json:"ingests"`
	}](f.t, f.apa(f.OBS, 0, "source", "ingests", source).Stdout).Ingests
}

func (f *fx) waitIngest(source, payloadID string) ingestRec {
	f.t.Helper()
	var got ingestRec
	f.waitFor("入站记录处理完成 "+payloadID, 30*time.Second, func() bool {
		for _, g := range f.ingests(source) {
			if g.State == "processed" && g.Body != nil && strings.Contains(*g.Body, `"`+payloadID+`"`) {
				got = g
				return true
			}
		}
		return false
	})
	return got
}

func (f *fx) ingestByID(source, id string) ingestRec {
	f.t.Helper()
	for _, g := range f.ingests(source) {
		if g.ID == id {
			return g
		}
	}
	f.t.Fatalf("入站记录 %s 不见了（元数据不应被清理）", id)
	return ingestRec{}
}

// sql 直接改账本：只用来把时间拨到过去（测试里无法等 30 天），不改任何业务内容。
func (f *fx) sqlExec(query string, args ...any) {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Home, "ledger.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		f.t.Fatalf("修改账本 %q: %v", query, err)
	}
}

func ago(d time.Duration) int64 { return time.Now().Add(-d).UnixNano() }

type runRec struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Target string `json:"target"`
}

func (f *fx) runs() map[string]runRec {
	f.t.Helper()
	m := map[string]runRec{}
	for _, r := range decode[struct {
		Runs []runRec `json:"runs"`
	}](f.t, f.apa(f.OBS, 0, "run", "list", "--category", "task").Stdout).Runs {
		m[r.ID] = r
	}
	return m
}

func (f *fx) runDir(id string) string { return filepath.Join(f.Home, "runs", id) }

func (f *fx) dirExists(p string) bool { _, err := os.Stat(p); return err == nil }

// newRuns 触发 n 次一次性任务型运行（回执后结束），返回等到结束的新 Run id。
func (f *fx) finishedRuns(n int) []string {
	f.t.Helper()
	known := f.runs()
	for i := 0; i < n; i++ {
		f.task(fmt.Sprintf("RET 一次性运行 %d", i), "acc-once")
	}
	var ids []string
	f.waitFor("一次性运行结束", 30*time.Second, func() bool {
		ids = ids[:0]
		for id, r := range f.runs() {
			if _, old := known[id]; !old && r.State == "succeeded" {
				ids = append(ids, id)
			}
		}
		return len(ids) == n
	})
	return ids
}

// retentionCase 在一个环境里跑 ACT-RET-01、ACT-RET-02：
// ingestDays/runDays 是该环境生效的保留天数（默认环境 30/14，调小后的环境 1/1），
// 数据按"保留期之内/之外"各拨一份过去的时间，外加一份刚到的与一个在途运行。
func retentionCase(t *testing.T, cfg string, ingestDays, runDays int) {
	f := opsFx(t, cfg)
	f.auto("once", "run-hold")
	f.addWebhookSource("src_acc_ret")
	tok := f.apa(f.DEL, 0, "source", "webhook", "create", "src_acc_ret", "--evidence", "验收签发").Stdout
	secret := decode[struct {
		Secret string `json:"secret"`
	}](t, tok).Secret

	var inside, outside, fresh ingestRec
	step(t, "ACT-RET-01 Ingest 请求体过期只留元数据", func(t *testing.T) {
		inside = f.postIngest("src_acc_ret", secret, "d-in", `{"id":"d-in","title":"保留期内"}`)
		outside = f.postIngest("src_acc_ret", secret, "d-out", `{"id":"d-out","title":"保留期外"}`)
		fresh = f.postIngest("src_acc_ret", secret, "d-new", `{"id":"d-new","title":"刚到的"}`)
		// 一份落在保留期之内（差 1 天到期），一份落在之外（已超 1 天）。
		f.sqlExec(`UPDATE webhook_ingest SET received_at = ? WHERE id = ?`, ago(time.Duration(ingestDays-1)*day+12*time.Hour), inside.ID)
		f.sqlExec(`UPDATE webhook_ingest SET received_at = ? WHERE id = ?`, ago(time.Duration(ingestDays+1)*day), outside.ID)

		f.waitFor("期外请求体被清理", sweepWait, func() bool { return f.ingestByID("src_acc_ret", outside.ID).Body == nil })
		got := f.ingestByID("src_acc_ret", outside.ID)
		if got.State != "processed" || got.DeliveryKey != outside.DeliveryKey || got.Events != outside.Events || got.BodySize != outside.BodySize || got.Signed != outside.Signed {
			t.Errorf("元数据应保留: 清理前 %+v，清理后 %+v", outside, got)
		}
		for name, rec := range map[string]ingestRec{"期内": inside, "刚到": fresh} {
			if g := f.ingestByID("src_acc_ret", rec.ID); g.Body == nil || *g.Body != *rec.Body {
				t.Errorf("%s的请求体不应被清理: %+v", name, g)
			}
		}
		r := f.Apa(f.DEL, "", "source", "replay", outside.ID, "--evidence", "验收重放")
		if r.Code == 0 {
			t.Errorf("请求体已清理的入站记录 replay 应明确报错: %s", r.Stdout)
		}
		f.apa(f.DEL, 0, "source", "replay", inside.ID, "--evidence", "验收重放")
	})

	step(t, "ACT-RET-02 运行目录过期清理，Run 记录和在途目录不动", func(t *testing.T) {
		ids := f.finishedRuns(3)
		in, out, newest := ids[0], ids[1], ids[2]
		for _, id := range ids {
			if !f.dirExists(f.runDir(id)) {
				t.Fatalf("运行目录应存在: %s", id)
			}
		}
		f.task("RET 在途运行", "acc-hold")
		var live string
		f.waitFor("在途运行开始", 30*time.Second, func() bool {
			for id, r := range f.runs() {
				if r.State == "running" {
					live = id
					return true
				}
			}
			return false
		})
		// 期内的差 1 天到期、期外的已超 1 天；在途运行的开始时间拨到远超保留期，目录也必须留着。
		f.sqlExec(`UPDATE outcall_run SET finished_at = ? WHERE id = ?`, ago(time.Duration(runDays-1)*day+12*time.Hour), in)
		f.sqlExec(`UPDATE outcall_run SET finished_at = ? WHERE id = ?`, ago(time.Duration(runDays+1)*day), out)
		f.sqlExec(`UPDATE outcall_run SET started_at = ? WHERE id = ?`, ago(time.Duration(runDays+30)*day), live)

		f.waitFor("期外的运行目录被清理", sweepWait, func() bool { return !f.dirExists(f.runDir(out)) })
		for name, id := range map[string]string{"期内": in, "刚结束": newest, "在途": live} {
			if !f.dirExists(f.runDir(id)) {
				t.Errorf("%s的运行目录不应被清理: %s", name, id)
			}
		}
		all := f.runs()
		for _, id := range []string{in, out, newest, live} {
			if _, ok := all[id]; !ok {
				t.Errorf("Run 记录不应丢失: %s", id)
			}
		}
		eq(t, "在途运行仍在运行", all[live].State, "running")
		// 账本事实还在：Run 记录可读，事项上该运行的 Event 仍可回放。
		f.apa(f.OBS, 0, "run", "show", out)
		if why := f.apa(f.OBS, 0, "why", all[out].Target).Stdout; !strings.Contains(why, "run.finished") {
			t.Errorf("账本里该运行的 Event 应仍可回放: %s", why)
		}
	})
}

func (f *fx) addWebhookSource(id string) {
	f.t.Helper()
	f.apa(f.DEL, 0, "source", "add", "--id", id, "--type", "webhook", "--source-type", "acc-ret", "--config", `{"scheme":"acc-token","mappings":["acc-generic"]}`, "--evidence", "验收登记")
}

// TestFlowRetention 对应验收文档 flow-retention（ACT-RET-01、ACT-RET-02）。
// 内核的保留期以整天为单位（ingest_retention_days、run_retention_days，最小 1 天），无法配成"1 分钟"，
// 所以：默认环境验证默认值（30 天、14 天）的边界；调小环境把两项都配成 1 天并验证按新值清理。
// 两个环境互相独立，并行执行；"过期"靠把账本里的时间拨到过去实现。
func TestFlowRetention(t *testing.T) {
	t.Run("默认保留期 30 天与 14 天", func(t *testing.T) {
		t.Parallel()
		retentionCase(t, "", 30, 14)
	})
	t.Run("配置调小为 1 天", func(t *testing.T) {
		t.Parallel()
		retentionCase(t, "ingest_retention_days: 1\nrun_retention_days: 1\n", 1, 1)
	})
}
