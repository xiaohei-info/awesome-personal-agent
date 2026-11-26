//go:build e2e

package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// newStub 与 TestUpgradeChain 同口径：模型替身一律答 503，待判断的事项保持待办。
func newStub(t *testing.T) *modelstub.Stub {
	s := modelstub.NewAt("127.0.0.1:0")
	s.SetDefault(modelstub.Reply{Status: 503})
	t.Cleanup(s.Close)
	return s
}

// TestSchema8Upgrade 是 flow-wb-schema8-upgrade 的全流程（ACT-S8-01..07）：结构版本 7 夹具副本由主干二进制升级到 8，
// 升级后行数不减、Event 序号连续且载荷不变、旧数据经新接口照常可读、旧二进制拒绝打开、升级前快照恢复后与升级前一致、
// 迁移中途失败整体回滚。只用合成夹具与旧版本二进制，模型端点是替身；Web 步骤用等价的 HTTP 调用。
func TestSchema8Upgrade(t *testing.T) {
	latest, err := ledgerfixture.Latest()
	if err != nil {
		t.Fatal(err)
	}
	r := &run{lab: newLab(t), v: 7, latest: latest}
	r.stub = newStub(t)

	r.step(t, "ACT-S8-00 结构版本 7 夹具与旧二进制", func(t *testing.T) {
		r.upg01(t)
		if r.m.SchemaLedger != 7 || len(r.m.Mail16K) < 3 || r.m.HostedMaterial == "" || r.m.OwnedMaterial == "" || len(r.m.GrantEvents) == 0 {
			t.Fatalf("夹具缺升级演练数据: schema=%d mail16k=%d hosted=%q owned=%q grants=%d",
				r.m.SchemaLedger, len(r.m.Mail16K), r.m.HostedMaterial, r.m.OwnedMaterial, len(r.m.GrantEvents))
		}
	})

	r.step(t, "ACT-S8-01 主干二进制打开夹具副本完成升级并留快照", func(t *testing.T) {
		r.upg04(t) // 启动、恰一份 pre-upgrade 快照且 backup verify 通过、apa version 与 /v1/version 都是 latest
		r.srv.stop()
	})

	r.step(t, "ACT-S8-02 行数不减、Event 序号连续、载荷不变、新列在旧行为空", func(t *testing.T) {
		db := openRO(t, r.home)
		counts := tableCounts(t, db)
		for tbl, n := range r.m.Tables {
			got, ok := counts[tbl]
			if !ok {
				t.Errorf("清单里的表 %s 不存在", tbl)
			} else if got < n && tbl != "followup_pending" { // 到期跟进被内核处理后删行，见 upg05
				t.Errorf("表 %s 行数减少：%d → %d", tbl, n, got)
			}
		}
		for _, tbl := range []string{"sources_runner_event_part", "item_import_batch", "item_import_map"} {
			if _, ok := counts[tbl]; !ok {
				t.Errorf("升级后缺新表 %s", tbl)
			}
		}
		all := events(t, db, "")
		for i, e := range all {
			if e.Seq != int64(i+1) {
				t.Fatalf("Event 序号不连续：第 %d 条是 %d", i+1, e.Seq)
			}
			if e.Seq <= r.m.EventMaxSeq && e.PayloadHashLower != r.m.EventPayloadSHA[itoa(int(e.Seq))] {
				t.Errorf("Event %d 载荷哈希变了", e.Seq)
			}
		}
		if int64(len(all)) < r.m.EventMaxSeq {
			t.Fatalf("Event 共 %d 条，少于升级前 %d", len(all), r.m.EventMaxSeq)
		}
		if n := one[int](t, db, `SELECT COUNT(*) FROM eventlog_event WHERE seq <= ? AND (import_batch <> '' OR recorded_at <> 0)`, r.m.EventMaxSeq); n != 0 {
			t.Errorf("%d 条已有 Event 的导入批次/实际写入时刻不为空", n)
		}
		if n := one[int](t, db, `SELECT COUNT(*) FROM sources_runner_event WHERE content_state <> 'legacy'`); n != 0 {
			t.Errorf("%d 条已有来源事件不是 legacy", n)
		}
	})

	r.step(t, "ACT-S8-03 升级前的 16 KiB 邮件事件标为 legacy 且照常可读", func(t *testing.T) {
		r.srv = r.serve(r.apaBin(), r.home)
		code, cookie, csrf := r.srv.webLogin(r.m.WebSecret)
		if code != 200 {
			t.Fatalf("Web 登录 %d", code)
		}
		db := openRO(t, r.home)
		for _, e := range r.m.Mail16K {
			x := r.srv.web(cookie, csrf, "GET", "/v1/source-events/"+e.ID, "")
			if x.Code != 200 {
				t.Fatalf("读 %s: %d %s", e.ID, x.Code, x.Body)
			}
			got := decode[struct {
				Content struct{ State string }
				Body    *string
				Scope   string
			}](t, x.Body)
			if got.Content.State != "legacy" || got.Scope != "full" || got.Body == nil {
				t.Errorf("事件 %s: state=%q scope=%q body=%v", e.ID, got.Content.State, got.Scope, got.Body != nil)
				continue
			}
			h := sha256.Sum256([]byte(*got.Body))
			if len(*got.Body) != 16384 || hex.EncodeToString(h[:]) != e.BodySHA256 {
				t.Errorf("事件 %s 的正文与升级前不同（%d 字节）", e.ID, len(*got.Body))
			}
			src := one[string](t, db, `SELECT source_id FROM sources_runner_event WHERE id = ?`, e.ID)
			st := decode[struct {
				Sources []struct {
					ID           string
					ContentBytes int64 `json:"content_bytes"`
				}
			}](t, r.srv.web(cookie, csrf, "GET", "/v1/storage", "").Body)
			found := false
			for _, s := range st.Sources {
				if s.ID == src {
					found = true
					if s.ContentBytes < 16384 {
						t.Errorf("/storage 来源 %s 的 content_bytes = %d，应按原列长度计入（≥16384）", src, s.ContentBytes)
					}
				}
			}
			if !found {
				t.Errorf("/storage 没有来源 %s", src)
			}
		}
	})

	r.step(t, "ACT-S8-04 材料形态按原规则推定且旧行不改写", func(t *testing.T) {
		for id, want := range map[string]string{r.m.HostedMaterial: "hosted", r.m.OwnedMaterial: "owned"} {
			x := r.srv.get(r.obs, "/v1/materials/"+id)
			if x.Code != 200 {
				t.Fatalf("读材料 %s: %d %s", id, x.Code, x.Body)
			}
			if form := decode[struct{ Material struct{ Form string } }](t, x.Body).Material.Form; form != want {
				t.Errorf("材料 %s 的 form = %q，期望 %q", id, form, want)
			}
			if raw := one[string](t, openRO(t, r.home), `SELECT form FROM materials WHERE id = ?`, id); raw != "" {
				t.Errorf("材料 %s 的形态列被改写为 %q", id, raw)
			}
		}
	})

	r.step(t, "ACT-S8-05 已有来源、事项与授权单事件不受新列影响", func(t *testing.T) {
		// 来源：全部已有来源的 content_retention_days 为空（不清理）。
		list := decode[struct {
			Sources []struct {
				ID   string
				Days *int `json:"content_retention_days"`
			}
		}](t, r.apa(r.home, r.extra(r.obs), 0, "source", "list").Stdout)
		if len(list.Sources) < len(r.m.Sources) {
			t.Fatalf("source list 只有 %d 个来源，夹具 %d 个", len(list.Sources), len(r.m.Sources))
		}
		for _, s := range list.Sources {
			if s.Days != nil {
				t.Errorf("已有来源 %s 被设了保留天数 %d", s.ID, *s.Days)
			}
		}
		// 事项 origin 查询：旧事项没有导入信息；对照不带过滤时事项存在，空结果才有意义。
		if n := len(decode[struct{ Items []struct{ ID string } }](t, r.srv.get(r.obs, "/v1/items").Body).Items); n == 0 {
			t.Fatal("夹具应有事项")
		}
		if x := r.srv.get(r.obs, "/v1/items?origin_source=import:any"); x.Code != 200 || len(decode[struct{ Items []struct{ ID string } }](t, x.Body).Items) != 0 {
			t.Errorf("origin_source=import:any 应返回空列表: %d %s", x.Code, x.Body)
		}
		// 审计 imported 筛选：已有 Event 都不属于任何导入批次。
		if n := len(decode[struct{ Events []struct{ Seq int64 } }](t, r.srv.get(r.obs, "/v1/audit/events?limit=5").Body).Events); n == 0 {
			t.Fatal("审计应有 Event")
		}
		if x := r.srv.get(r.obs, "/v1/audit/events?imported=imp_none"); x.Code != 200 || len(decode[struct{ Events []struct{ Seq int64 } }](t, x.Body).Events) != 0 {
			t.Errorf("imported=imp_none 应返回空列表: %d %s", x.Code, x.Body)
		}
		// 带授权单的 Event 回放仍显示原授权单 id。
		for _, g := range r.m.GrantEvents {
			res := r.apa(r.home, r.extra(r.obs), 0, "why", g.Object)
			why := decode[struct{ Steps []struct{ Seq int64 } }](t, res.Stdout)
			if len(why.Steps) == 0 {
				t.Errorf("apa why %s 没有时间线: %s", g.Object, res.Stdout)
			}
			contains(t, "apa why "+g.Object, res.Stdout, g.Grant)
		}
		r.srv.stop()
	})

	r.step(t, "ACT-S8-06 旧二进制拒绝打开，快照恢复后与升级前一致", func(t *testing.T) {
		ledger := filepath.Join(r.home, "ledger.db")
		before := fileSHA(t, ledger)
		if s, res := r.tryServeAsIs(r.bin, r.home); s != nil || res.Code == 0 {
			t.Fatalf("旧二进制不应能打开结构版本 8 的账本（退出码 %d）: %s", res.Code, res.Stdout)
		}
		if after := fileSHA(t, ledger); after != before {
			t.Errorf("旧二进制拒绝启动时改动了 ledger.db")
		}
		var snap string
		for _, s := range r.backups(r.home) {
			if strings.Contains(filepath.Base(s), "pre-upgrade") {
				snap = s
			}
		}
		if snap == "" {
			t.Fatalf("没有升级前快照: %v", r.backups(r.home))
		}
		restore := filepath.Join(r.Root, "fx-restore")
		r.apa(r.home, nil, 0, "backup", "restore", snap, "--home", restore)
		// 恢复结果与升级前一致：在旧二进制启动（它会写自己的例行维护）之前核对。
		db := openRO(t, restore)
		if got := tableCounts(t, db); fmt.Sprint(got) != fmt.Sprint(r.m.Tables) {
			t.Errorf("恢复后各表行数与清单不一致:\n清单 %v\n恢复 %v", r.m.Tables, got)
		}
		if max := one[int64](t, db, `SELECT MAX(seq) FROM eventlog_event`); max != r.m.EventMaxSeq {
			t.Errorf("恢复后最大 Event 序号 = %d，期望 %d", max, r.m.EventMaxSeq)
		}
		for _, e := range events(t, db, "") {
			if e.PayloadHashLower != r.m.EventPayloadSHA[itoa(int(e.Seq))] {
				t.Errorf("恢复后 Event %d 载荷变了", e.Seq)
			}
		}
		if n := one[int](t, db, `SELECT COUNT(*) FROM pragma_table_info('eventlog_event') WHERE name = 'import_batch'`); n != 0 {
			t.Errorf("恢复后的账本仍带结构版本 8 的列")
		}
		// 旧二进制能打开恢复出的目录。
		old := r.serve(r.bin, restore)
		if res := r.run(r.bin, restore, []string{"APA_URL=" + old.URL, "APA_TOKEN=" + r.obs}, "inbox"); res.Code != 0 {
			t.Errorf("旧二进制应能列出待办: %d %s%s", res.Code, res.Stdout, res.Stderr)
		}
		old.stop()
	})

	r.step(t, "ACT-S8-07 迁移中途失败整体回滚", r.upg15)
}
