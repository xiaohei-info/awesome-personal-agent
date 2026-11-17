//go:build e2e

package importwb

import (
	"fmt"
	"strings"
	"time"
)

// 合成的计划文件（验收文档 ACT-IMP-01）。时刻都相对内核"现在"向前推，使它们总在过去；
// 固定写成 +08:00，导入后按输入时刻逐条比较。
var cst = time.FixedZone("CST", 8*3600)

type planFixture struct {
	t0         time.Time // T-0001 的 created（原文档的 10-01 09:00）
	t1Done     time.Time // T-0001 转 done 的时刻（原文档的 10-09 18:00）
	alphaAt    time.Time // Alpha Imp 的 created_at（原文档的 03-02 09:00）
	t2Created  time.Time
	t2Waiting  time.Time
	progressAt []time.Time // 五条进展的 at
	progressOn []string    // 五条进展的 on
	deadline   time.Time
}

// planAt 按内核此刻定出各时刻：T-0001 从 40 天前起，每天一步。
func (f *fx) planAt() planFixture {
	y, m, d := f.now().In(cst).Date()
	day := func(back, hour int) time.Time { return time.Date(y, m, d-back, hour, 0, 0, 0, cst) }
	p := planFixture{t0: day(40, 9), alphaAt: day(200, 9), t1Done: day(32, 18), t2Created: day(31, 9), t2Waiting: day(30, 9),
		deadline: f.now().Add(-24 * time.Hour).Truncate(time.Second)}
	for i := 0; i < 5; i++ {
		hour := 12
		if i >= 3 { // 后两条给精确时刻
			hour = 15 + i
		}
		at := day(39-i, hour)
		p.progressAt = append(p.progressAt, at)
		p.progressOn = append(p.progressOn, at.Format("2006-01-02"))
	}
	return p
}

func rfc(t time.Time) string { return t.Format(time.RFC3339) }

// plan 是 ACT-IMP-01 的四行计划文件。
func (f *fx) plan() string {
	p := f.planAt()
	prog := func(i int, text string) string {
		prec := ""
		if i < 3 {
			prec = `,"precision":"day"`
		}
		return fmt.Sprintf(`{"at":%q,"event":"progress","text":%q,"on":%q%s}`, rfc(p.progressAt[i]), text, p.progressOn[i], prec)
	}
	hist1 := []string{
		fmt.Sprintf(`{"at":%q,"event":"created","phase":"open"}`, rfc(p.t0)),
		prog(0, "进展一"), prog(1, "进展二"), prog(2, "进展三"), prog(3, "进展四"), prog(4, "进展五"),
		fmt.Sprintf(`{"at":%q,"event":"transition","to":"dropped","reason":"暂时放弃"}`, rfc(p.t0.AddDate(0, 0, 6))),
		fmt.Sprintf(`{"at":%q,"event":"transition","to":"open","reason":"重新开始"}`, rfc(p.t0.AddDate(0, 0, 7))),
		fmt.Sprintf(`{"at":%q,"event":"transition","to":"done","precision":"day"}`, rfc(p.t1Done)),
	}
	lines := []string{
		fmt.Sprintf(`{"type":"project","external_id":"acc/alpha","name":"Alpha Imp","status":"active","created_at":%q}`, rfc(p.alphaAt)),
		`{"type":"project","external_id":"acc/home","name":"  ACC-HOME "}`,
		fmt.Sprintf(`{"type":"item","external_id":"T-0001","project":"acc/alpha","item":{"kind":"task","title":"导入的已完成事项","deadline":null},"history":[%s]}`, strings.Join(hist1, ",")),
		fmt.Sprintf(`{"type":"item","external_id":"T-0002","project":"acc/home","item":{"kind":"task","title":"导入的等待事项","deadline":%q,"waiting_on":{"kind":"human","ref":"某人"}},"history":[{"at":%q,"event":"created","phase":"open"},{"at":%q,"event":"transition","to":"waiting"}]}`,
			rfc(p.deadline), rfc(p.t2Created), rfc(p.t2Waiting)),
	}
	return strings.Join(lines, "\n") + "\n"
}

// simpleItems 是 n 条各自一个创建记录的合法事项（授权单流程与分批用），外部 id 为 prefix-序号。
func (f *fx) simpleItems(prefix string, n int) string {
	at := rfc(f.now().Add(-72 * time.Hour).In(cst).Truncate(time.Second))
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `{"type":"item","external_id":"%s-%d","item":{"kind":"task","title":"导入 %s %d"},"history":[{"at":%q,"event":"created","phase":"open"}]}`+"\n", prefix, i, prefix, i, at)
	}
	return b.String()
}
