package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// upstream 按真实接口的语义模拟 GitHub：issue 列表按 updated_at 升序、since 含端点；
// 通知按 updated_at 倒序、before 不含端点；都按 per_page、page 分页。
type upstream struct {
	issues map[int]string // number → 当前状态的 JSON
	notifs []string
	err    error
	calls  []string
	onGet  func(call int) // 每次请求前调用，用来模拟读取途中的上游变化
	agents *string        // 默认分支根目录 AGENTS.md 的 blob sha；nil 表示没有该文件（404）
}

// issueCalls 是对 issue 列表的请求（AGENTS.md 读取不算）。
func (u *upstream) issueCalls() []string {
	return slices.DeleteFunc(slices.Clone(u.calls), func(c string) bool { return strings.Contains(c, "/contents/") })
}

func (u *upstream) set(n int, raw string) {
	if u.issues == nil {
		u.issues = map[int]string{}
	}
	u.issues[n] = raw
}

func (u *upstream) Get(_ context.Context, path string) ([]byte, error) {
	u.calls = append(u.calls, path)
	if u.onGet != nil {
		u.onGet(len(u.calls))
	}
	if u.err != nil {
		return nil, u.err
	}
	p, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(p.Path, "/contents/AGENTS.md") {
		if u.agents == nil {
			return nil, &Error{Exit: ExitAuth, Msg: "gh: Not Found (HTTP 404)", NotFound: true}
		}
		return []byte(fmt.Sprintf(`{"type":"file","sha":%q,"html_url":"https://github.com/o/r/blob/main/AGENTS.md"}`, *u.agents)), nil
	}
	q := p.Query()
	var all []string
	if strings.HasPrefix(path, "/notifications") {
		all = u.notifs
	} else {
		for _, raw := range u.issues {
			all = append(all, raw)
		}
	}
	since, _ := time.Parse(time.RFC3339, q.Get("since"))
	before, _ := time.Parse(time.RFC3339, q.Get("before"))
	type row struct {
		at  time.Time
		key string
		raw string
	}
	var rows []row
	for _, raw := range all {
		var it struct {
			UpdatedAt time.Time `json:"updated_at"`
			Number    int       `json:"number"`
			ID        string    `json:"id"`
		}
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			return nil, err
		}
		if it.UpdatedAt.Before(since) || (!before.IsZero() && !it.UpdatedAt.Before(before)) {
			continue
		}
		rows = append(rows, row{it.UpdatedAt, fmt.Sprintf("%08d%s", it.Number, it.ID), raw})
	}
	desc := q.Get("direction") != "asc"
	slices.SortFunc(rows, func(a, b row) int {
		c := a.at.Compare(b.at)
		if c == 0 {
			c = strings.Compare(a.key, b.key)
		}
		if desc {
			return -c
		}
		return c
	})
	per, _ := strconv.Atoi(q.Get("per_page"))
	page, _ := strconv.Atoi(q.Get("page"))
	var out []string
	for i := (page - 1) * per; i < page*per && i < len(rows); i++ {
		out = append(out, rows[i].raw)
	}
	return []byte("[" + strings.Join(out, ",") + "]"), nil
}

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func now() time.Time { return t0.Add(48 * time.Hour) }

func at(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

// item 生成 issue 列表项；closed、merged 为空表示没有。
func item(n int, created, updated, closed, merged, body string) string {
	m := map[string]any{"number": n, "title": fmt.Sprintf("t%d", n), "body": body, "html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", n),
		"created_at": created, "updated_at": updated, "user": map[string]string{"login": "alice"}}
	if closed != "" {
		m["closed_at"] = closed
	}
	if merged != "" {
		m["pull_request"] = map[string]string{"merged_at": merged}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func notif(id int, updated string) string {
	return fmt.Sprintf(`{"id":"%d","reason":"mention","updated_at":"%s","repository":{"full_name":"o/r"},"subject":{"title":"s","type":"Issue"}}`, id, updated)
}

type output struct {
	events []map[string]any
	cursor string
	bytes  int
}

func (o output) ids() []string {
	var ids []string
	for _, ev := range o.events {
		ids = append(ids, ev["id"].(string))
	}
	return ids
}

// pullOnce 跑一次；首次（cursor 为空）从 t0 起算。
func pullOnce(t *testing.T, api API, cfg, cursor string) output {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"source_id": "s", "config": json.RawMessage(cfg), "cursor": cursor, "window": map[string]string{"from": at(0)}})
	var out, errb bytes.Buffer
	if code := Run(context.Background(), bytes.NewReader(in), &out, &errb, api, now); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var o output
	o.bytes = out.Len()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for _, l := range lines[:len(lines)-1] {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line not JSON: %q", l)
		}
		o.events = append(o.events, m)
	}
	var last map[string]string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || len(last) != 1 || last["cursor"] == "" {
		t.Fatalf("last line %q", lines[len(lines)-1])
	}
	o.cursor = last["cursor"]
	return o
}

const repoCfg = `{"repos":["o/r"]}`

// drain 反复把游标喂回去直到没有新输出，返回事件 id 的出现次数；要求每次有积压的运行都有进展。
func drain(t *testing.T, api API, cfg string) (map[string]int, string) {
	t.Helper()
	seen := map[string]int{}
	cursor := ""
	for range 200 {
		o := pullOnce(t, api, cfg, cursor)
		for _, id := range o.ids() {
			seen[id]++
		}
		if len(o.events) == 0 && o.cursor == cursor {
			return seen, cursor
		}
		cursor = o.cursor
	}
	t.Fatalf("no fixed point after 200 runs: %d ids, cursor %s", len(seen), cursor[:min(len(cursor), 200)])
	return nil, ""
}

func TestPullMapsIssuesPRsNotificationsAndAdvancesCursor(t *testing.T) {
	api := &upstream{notifs: []string{`{"id":"77","reason":"review_requested","updated_at":"2026-11-01T15:00:00Z","repository":{"full_name":"o/r"},"subject":{"title":"Please review","type":"PullRequest","url":"https://api.github.com/repos/o/r/pulls/9"}}`}}
	old := "2026-10-01T00:00:00Z"
	api.set(1, item(1, old, at(10*time.Hour), "", "", "b"))
	api.set(2, item(2, at(11*time.Hour), at(11*time.Hour), "", "", "hello"))
	api.set(3, item(3, old, at(13*time.Hour), at(13*time.Hour), at(13*time.Hour), ""))
	api.set(4, item(4, old, at(14*time.Hour), at(14*time.Hour), "", ""))
	o := pullOnce(t, api, `{"repos":["o/r"],"notifications":true}`, "")
	want := []struct{ id, typ string }{
		{"issue-o-r#1-updated-" + fmt.Sprint(t0.Add(10*time.Hour).Unix()), "issue.updated"},
		{"issue-o-r#2-opened", "issue.opened"},
		{"pr-o-r#3-merged", "pr.merged"},
		{"issue-o-r#4-closed-" + fmt.Sprint(t0.Add(14*time.Hour).Unix()), "issue.closed"},
		{"notif-77-" + fmt.Sprint(t0.Add(15*time.Hour).Unix()), "notification.review_requested"},
	}
	if len(o.events) != len(want) {
		t.Fatalf("events %v", o.ids())
	}
	for i, w := range want {
		if ev := o.events[i]; ev["id"] != w.id || ev["type"] != w.typ || ev["subject"] == "" || ev["occurred_at"] == "" {
			t.Errorf("event %d = %v, want %v", i, ev, w)
		}
	}
	if ev := o.events[1]; ev["summary"] != "hello" || ev["correlation_keys"].([]any)[0] != "o/r#2" {
		t.Errorf("issue fields: %v", ev)
	}
	if ev := o.events[4]; ev["link"] != "https://github.com/o/r/pull/9" || ev["correlation_keys"].([]any)[0] != "o/r#9" {
		t.Errorf("notification fields: %v", ev)
	}
	var cur map[string]stream
	if err := json.Unmarshal([]byte(o.cursor), &cur); err != nil {
		t.Fatal(err)
	}
	if !cur["o/r"].At.Equal(t0.Add(14*time.Hour)) || !cur[notifStream].At.Equal(t0.Add(15*time.Hour)) {
		t.Errorf("cursor %s", o.cursor)
	}
}

// 评审第 1 轮 event-type-derived-from-cursor：同一上游状态、把上次游标喂回去，不产生任何新事件，游标也不变；
// 同一游标重放，输出完全相同。
func TestCursorReplayKeepsIDsAndEmitsNothingNew(t *testing.T) {
	api := &upstream{}
	api.set(2, item(2, at(11*time.Hour), at(12*time.Hour), "", "", "x"))
	api.set(3, item(3, at(10*time.Hour), at(13*time.Hour+5*time.Minute), at(13*time.Hour), at(13*time.Hour), ""))
	first := pullOnce(t, api, repoCfg, "")
	if again := pullOnce(t, api, repoCfg, ""); !slices.Equal(first.ids(), again.ids()) || first.cursor != again.cursor {
		t.Fatalf("same cursor, different output: %v vs %v", first.ids(), again.ids())
	}
	second := pullOnce(t, api, repoCfg, first.cursor)
	if len(second.events) != 0 || second.cursor != first.cursor {
		t.Fatalf("replay emitted %v, cursor %s -> %s", second.ids(), first.cursor, second.cursor)
	}
}

// 评审第 1 轮：一个窗口内创建并合并的 PR，opened 和 merged 都要有。
func TestCreatedAndMergedInOneWindowYieldsBoth(t *testing.T) {
	api := &upstream{}
	api.set(5, item(5, at(time.Hour), at(2*time.Hour), at(2*time.Hour), at(2*time.Hour), ""))
	o := pullOnce(t, api, repoCfg, "")
	if len(o.events) != 2 || o.events[0]["type"] != "pr.opened" || o.events[1]["type"] != "pr.merged" {
		t.Fatalf("events %v", o.ids())
	}
}

// 事件集合只由上游状态决定：无论游标落在 opened 和 updated 之间的哪里，updated 都是同一个 id。
func TestUpdatedIDIndependentOfCursor(t *testing.T) {
	api := &upstream{}
	api.set(2, item(2, at(time.Hour), at(2*time.Hour), "", "", "x"))
	want := fmt.Sprintf("issue-o-r#2-updated-%d", t0.Add(2*time.Hour).Unix())
	from0 := pullOnce(t, api, repoCfg, "")
	if !slices.Equal(from0.ids(), []string{"issue-o-r#2-opened", want}) {
		t.Fatalf("from window start: %v", from0.ids())
	}
	mid, _ := json.Marshal(map[string]stream{"o/r": {At: t0.Add(90 * time.Minute), Floor: t0.Add(90 * time.Minute)}})
	if got := pullOnce(t, api, repoCfg, string(mid)); !slices.Equal(got.ids(), []string{want}) {
		t.Fatalf("from mid: %v", got.ids())
	}
}

// 评审第 2 轮 nit：close → reopen → close，两次关闭是两条事件，先前的不重发。
func TestCloseReopenCloseGetsDistinctIDs(t *testing.T) {
	api := &upstream{}
	old := "2026-10-01T00:00:00Z"
	api.set(7, item(7, old, at(time.Hour), at(time.Hour), "", ""))
	o1 := pullOnce(t, api, repoCfg, "")
	api.set(7, item(7, old, at(2*time.Hour), "", "", "")) // reopen：closed_at 清空，表现为 updated
	o2 := pullOnce(t, api, repoCfg, o1.cursor)
	api.set(7, item(7, old, at(3*time.Hour), at(3*time.Hour), "", ""))
	o3 := pullOnce(t, api, repoCfg, o2.cursor)
	if len(o1.events) != 1 || o1.events[0]["type"] != "issue.closed" ||
		len(o2.events) != 1 || o2.events[0]["type"] != "issue.updated" ||
		len(o3.events) != 1 || o3.events[0]["type"] != "issue.closed" || o3.ids()[0] == o1.ids()[0] {
		t.Fatalf("%v %v %v", o1.ids(), o2.ids(), o3.ids())
	}
}

// 评审第 2 轮 output-exceeds-runner-retained-limit：积压的输出超过 64KB 也在一次运行里全部发出，不再截断。
func TestLargeBacklogEmittedInOneRun(t *testing.T) {
	body := strings.Repeat("x", 3900)
	api := &upstream{}
	for i := 1; i <= 30; i++ {
		api.set(i, item(i, "2026-10-01T00:00:00Z", at(time.Duration(i)*time.Minute), "", "", body))
	}
	o := pullOnce(t, api, repoCfg, "")
	if len(o.events) != 30 || o.bytes <= 64<<10 {
		t.Fatalf("events %d, bytes %d", len(o.events), o.bytes)
	}
	if again := pullOnce(t, api, repoCfg, o.cursor); len(again.events) != 0 {
		t.Fatalf("second run emitted %v", again.ids())
	}
}

// 评审第 3 轮：同一秒的一组更新跨过单次额度时，被留到下次的对象的 updated 不能丢。
func TestSameSecondGroupAcrossItemCapLosesNothing(t *testing.T) {
	api := &upstream{}
	total := maxItems + 2*perPage
	for i := 1; i <= total; i++ {
		api.set(i, item(i, "2026-10-01T00:00:00Z", at(time.Hour), "", "", "")) // 只有 updated，全部同一秒
	}
	seen, _ := drain(t, api, repoCfg)
	if len(seen) != total {
		t.Fatalf("emitted %d distinct ids, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s emitted %d times", id, n)
		}
	}
}

// 评审第 4 轮：两组窗口内新建的同秒对象（opened 会被重新生成），原复现用例 10 次运行只发出 6/12 且游标不动。
func TestNewItemsInSameSecondGroupsDoNotLivelock(t *testing.T) {
	body := strings.Repeat("x", 3900)
	api := &upstream{}
	for i := 1; i <= 12; i++ {
		ts := at(time.Hour + time.Duration((i-1)/6)*time.Second)
		api.set(i, item(i, ts, ts, "", "", body))
	}
	seen, _ := drain(t, api, repoCfg)
	if len(seen) != 12 {
		t.Fatalf("emitted %d of 12 ids", len(seen))
	}
}

// 同样的场景放大到超过单次额度：游标每次都前进，全部发出，每个 id 只发一次。
func TestNewItemBacklogBeyondCapMakesProgressEveryRun(t *testing.T) {
	api := &upstream{}
	total := 3 * maxItems
	for i := 1; i <= total; i++ {
		ts := at(time.Hour + time.Duration((i-1)/(maxItems/2))*time.Second)
		api.set(i, item(i, ts, ts, "", "", ""))
	}
	cursor := ""
	seen := map[string]int{}
	for run := 0; ; run++ {
		o := pullOnce(t, api, repoCfg, cursor)
		for _, id := range o.ids() {
			seen[id]++
		}
		if len(o.events) == 0 {
			break
		}
		if len(o.events) > maxItems+perPage {
			t.Fatalf("run %d processed %d items", run, len(o.events))
		}
		if o.cursor == cursor || run > 10 {
			t.Fatalf("run %d: no progress, %d ids", run, len(seen))
		}
		cursor = o.cursor
	}
	if len(seen) != total {
		t.Fatalf("emitted %d of %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s emitted %d times", id, n)
		}
	}
}

// 积压分多次扫描时，排在后面的对象的 opened、closed 可能早于已推进的位置，不能因此丢掉。
func TestEarlyTransitionsSurviveMultiRunSweep(t *testing.T) {
	api := &upstream{}
	for i := 1; i <= maxItems+perPage; i++ {
		api.set(i, item(i, "2026-10-01T00:00:00Z", at(time.Hour+time.Duration(i)*time.Second), "", "", ""))
	}
	const late = 9999
	created, closed := at(2*time.Minute), at(3*time.Minute)
	api.set(late, item(late, created, at(10*time.Hour), closed, "", ""))
	seen, _ := drain(t, api, repoCfg)
	for _, id := range []string{fmt.Sprintf("issue-o-r#%d-opened", late), fmt.Sprintf("issue-o-r#%d-closed-%d", late, t0.Add(3*time.Minute).Unix())} {
		if seen[id] != 1 {
			t.Errorf("%s emitted %d times", id, seen[id])
		}
	}
}

// 游标所在这一秒后到的对象（同秒歧义）照发，已发的不重发。
func TestLateItemInCursorSecondIsEmitted(t *testing.T) {
	api := &upstream{}
	api.set(1, item(1, "2026-10-01T00:00:00Z", at(time.Hour), "", "", ""))
	o1 := pullOnce(t, api, repoCfg, "")
	api.set(2, item(2, at(time.Hour), at(time.Hour), "", "", ""))
	o2 := pullOnce(t, api, repoCfg, o1.cursor)
	if !slices.Equal(o2.ids(), []string{"issue-o-r#2-opened"}) {
		t.Fatalf("got %v", o2.ids())
	}
}

// 读取途中被更新的对象移到队尾，页码翻页会漏掉前移的对象；按 updated_at 续读不会。
func TestItemMovedDuringPagingDoesNotHideOthers(t *testing.T) {
	api := &upstream{}
	total := perPage + 10
	for i := 1; i <= total; i++ {
		api.set(i, item(i, "2026-10-01T00:00:00Z", at(time.Hour+time.Duration(i)*time.Minute), "", "", ""))
	}
	moved := false
	api.onGet = func(call int) {
		if call == 2 && !moved { // 读完第一页后，第 1 个对象被更新（仍在窗口内）
			moved = true
			api.set(1, item(1, "2026-10-01T00:00:00Z", at(40*time.Hour), "", "", ""))
		}
	}
	seen, _ := drain(t, api, repoCfg)
	for i := 2; i <= total; i++ {
		if seen[fmt.Sprintf("issue-o-r#%d-updated-%d", i, t0.Add(time.Hour+time.Duration(i)*time.Minute).Unix())] != 1 {
			t.Fatalf("item %d missing", i)
		}
	}
}

// 晚于 window.to 的对象留给下次；它较早的迁移不丢。
func TestWindowToLeavesLaterItemsForNextRun(t *testing.T) {
	api := &upstream{}
	api.set(1, item(1, "2026-10-01T00:00:00Z", at(time.Hour), "", "", ""))
	api.set(2, item(2, at(2*time.Minute), at(3*time.Hour), "", "", ""))
	in := fmt.Sprintf(`{"source_id":"s","config":%s,"window":{"from":%q,"to":%q}}`, repoCfg, at(0), at(2*time.Hour))
	var out bytes.Buffer
	if code := Run(context.Background(), strings.NewReader(in), &out, &bytes.Buffer{}, api, now); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out.String(), "#2") {
		t.Fatalf("later item emitted: %s", out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var last map[string]string
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	o := pullOnce(t, api, repoCfg, last["cursor"])
	if !slices.Contains(o.ids(), "issue-o-r#2-opened") {
		t.Fatalf("opened lost: %v", o.ids())
	}
}

// 多个仓库共用单次额度：前面的仓库积压再多，后面的仓库也轮得到。
func TestLaterRepoGetsItsTurn(t *testing.T) {
	big, small := &upstream{}, &upstream{}
	for i := 1; i <= maxItems+1; i++ {
		big.set(i, item(i, "2026-10-01T00:00:00Z", at(time.Hour+time.Duration(i)*time.Second), "", "", ""))
	}
	small.set(1, item(1, at(time.Hour), at(time.Hour), "", "", ""))
	api := routedAPI{"/repos/a/big/": big, "/repos/b/small/": small}
	seen, _ := drain(t, api, `{"repos":["a/big","b/small"]}`)
	if len(seen) != maxItems+2 || seen["issue-b-small#1-opened"] != 1 {
		t.Fatalf("emitted %d ids, small=%d", len(seen), seen["issue-b-small#1-opened"])
	}
}

type routedAPI map[string]*upstream

func (r routedAPI) Get(ctx context.Context, path string) ([]byte, error) {
	for prefix, u := range r {
		if strings.HasPrefix(path, prefix) {
			return u.Get(ctx, path)
		}
	}
	return []byte("[]"), nil
}

// 评审各轮 nit：通知流超量不再永久卡住（原来每次都按临时失败）；从最旧的开始发，游标前进，全部发出且不重复。
func TestNotificationBacklogDrainsAcrossRuns(t *testing.T) {
	api := &upstream{}
	total := maxNotifPages*perPage + 700
	for i := 0; i < total; i++ {
		api.notifs = append(api.notifs, notif(i, at(time.Hour+time.Duration(i)*time.Minute/4)))
	}
	seen, _ := drain(t, api, `{"notifications":true}`)
	if len(seen) != total {
		t.Fatalf("emitted %d of %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s emitted %d times", id, n)
		}
	}
}

// 同一秒的通知超过一次能读的量：时间窗无法再缩小时不设页数上限，也能走完。
func TestNotificationsInOneSecondBeyondPageCap(t *testing.T) {
	api := &upstream{}
	total := maxNotifPages*perPage + 50
	for i := 0; i < total; i++ {
		api.notifs = append(api.notifs, notif(i, at(time.Hour)))
	}
	seen, _ := drain(t, api, `{"notifications":true}`)
	if len(seen) != total {
		t.Fatalf("emitted %d of %d", len(seen), total)
	}
}

// 重放通知游标不产生新事件；同秒后到的照发。
func TestNotificationCursorReplayAndLateArrival(t *testing.T) {
	api := &upstream{notifs: []string{notif(1, at(time.Hour))}}
	o1 := pullOnce(t, api, `{"notifications":true}`, "")
	if o2 := pullOnce(t, api, `{"notifications":true}`, o1.cursor); len(o1.events) != 1 || len(o2.events) != 0 {
		t.Fatalf("%v %v", o1.ids(), o2.ids())
	}
	api.notifs = append(api.notifs, notif(2, at(time.Hour)))
	if o3 := pullOnce(t, api, `{"notifications":true}`, o1.cursor); len(o3.events) != 1 || !strings.HasPrefix(o3.ids()[0], "notif-2-") {
		t.Fatalf("%v", o3.ids())
	}
}

// 评审第 2 轮 nit：只有 opened 的动作者确定是作者才标 sender_verified。
func TestOnlyOpenedMarksSenderVerified(t *testing.T) {
	api := &upstream{}
	api.set(1, item(1, at(time.Hour), at(5*time.Hour), at(2*time.Hour), "", ""))
	api.set(2, item(2, at(time.Hour), at(2*time.Hour), at(2*time.Hour), at(2*time.Hour), ""))
	o := pullOnce(t, api, repoCfg, "")
	if len(o.events) != 5 {
		t.Fatalf("events %v", o.ids())
	}
	for _, ev := range o.events {
		verified, _ := ev["sender_verified"].(bool)
		if opened := strings.HasSuffix(ev["type"].(string), ".opened"); verified != opened || ev["sender"] != "alice" {
			t.Errorf("%v", ev)
		}
	}
}

func TestNoChangeKeepsCursor(t *testing.T) {
	o1 := pullOnce(t, &upstream{}, repoCfg, "")
	o2 := pullOnce(t, &upstream{}, repoCfg, o1.cursor)
	if len(o1.events)+len(o2.events) != 0 || o1.cursor != o2.cursor {
		t.Fatalf("%v %v", o1, o2)
	}
}

func TestFirstRunStartsFromWindowThenNow(t *testing.T) {
	api := &upstream{}
	run := func(in string) string {
		var out bytes.Buffer
		if code := Run(context.Background(), strings.NewReader(in), &out, &bytes.Buffer{}, api, now); code != ExitOK {
			t.Fatalf("exit %d", code)
		}
		return out.String()
	}
	run(`{"source_id":"s","config":{"repos":["o/r"]},"window":{"from":"2026-10-30T00:00:00Z"}}`)
	if !strings.Contains(api.issueCalls()[0], "since=2026-10-30T00:00:00Z") {
		t.Errorf("window.from ignored: %s", api.calls[0])
	}
	run(`{"source_id":"s","config":{"repos":["o/r"]}}`)
	if !strings.Contains(api.issueCalls()[1], "since=2026-11-03T00:00:00Z") {
		t.Errorf("first run without window should start now: %s", api.calls[1])
	}
}

func TestFailureExitCodesAndNoStdout(t *testing.T) {
	input := `{"source_id":"s","config":{"repos":["o/r"]}}`
	cases := []struct {
		name string
		err  error
		in   string
		want int
	}{
		{"auth", classify("HTTP 401: Bad credentials"), input, ExitAuth},
		{"not logged in", classify("To get started with GitHub CLI, please run:  gh auth login"), input, ExitAuth},
		{"private repo", classify("gh: Not Found (HTTP 404)"), input, ExitAuth},
		{"rate limit", classify("gh: API rate limit exceeded (HTTP 403)"), input, ExitTemporary},
		{"server", classify("gh: Bad Gateway (HTTP 502)"), input, ExitTemporary},
		{"network", classify("dial tcp: lookup api.github.com: no such host"), input, ExitTemporary},
		{"gone", classify("gh: Gone (HTTP 410)"), input, ExitPermanent},
		{"plain error", fmt.Errorf("boom"), input, ExitTemporary},
		{"no config", nil, `{"source_id":"s","config":{}}`, ExitAuth},
		{"bad repo", nil, `{"source_id":"s","config":{"repos":["../x"]}}`, ExitAuth},
		{"bad cursor", nil, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"x"}`, ExitAuth},
		{"bad json", nil, `{`, ExitAuth},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		code := Run(context.Background(), strings.NewReader(c.in), &out, &errb, &upstream{err: c.err}, now)
		if code != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, code, c.want)
		}
		if out.Len() != 0 || errb.Len() == 0 {
			t.Errorf("%s: stdout=%q stderr=%q", c.name, out.String(), errb.String())
		}
	}
}

type rawAPI string

func (r rawAPI) Get(context.Context, string) ([]byte, error) { return []byte(r), nil }

func TestMalformedResponseIsTemporary(t *testing.T) {
	for _, tc := range []struct {
		cfg  string
		body rawAPI
	}{{repoCfg, `{"message":"x"}`}, {repoCfg, `[{"number":0}]`}, {`{"notifications":true}`, `{"message":"x"}`}, {`{"notifications":true}`, `[{"id":""}]`}} {
		in := fmt.Sprintf(`{"source_id":"s","config":%s}`, tc.cfg)
		if code := Run(context.Background(), strings.NewReader(in), &bytes.Buffer{}, &bytes.Buffer{}, tc.body, now); code != ExitTemporary {
			t.Errorf("%s: exit %d", tc.body, code)
		}
	}
}

// 用假 gh 脚本走真实的 exec 路径：验证凭据目录隔离、环境 token 不泄入、失败分类。
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGHUsesGivenCredentialDirAndIgnoresEnvToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "leak")
	t.Setenv("GITHUB_TOKEN", "leak")
	t.Setenv("GH_HOST", "leak.example")
	bin := fakeGH(t, `[ "$1" = api ] || exit 9
[ -z "$GH_TOKEN$GITHUB_TOKEN$GH_HOST" ] || { echo "HTTP 500 env leaked" >&2; exit 1; }
echo "{\"dir\":\"$GH_CONFIG_DIR\"}" | sed 's/^/[/;s/$/]/'`)
	g := GH{Bin: bin, ConfigDir: "/x/sources/src_github/gh"}
	out, err := g.Get(context.Background(), "/repos/o/r/issues?page=1")
	if err != nil || !strings.Contains(string(out), "/x/sources/src_github/gh") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestGHFailureClassification(t *testing.T) {
	g := GH{Bin: fakeGH(t, `echo "gh: Bad credentials (HTTP 401)" >&2; exit 1`)}
	_, err := g.Get(context.Background(), "/x")
	if e, ok := err.(*Error); !ok || e.Exit != ExitAuth {
		t.Fatalf("err = %v", err)
	}
	_, err = GH{Bin: "/nonexistent/gh"}.Get(context.Background(), "/x")
	if e, ok := err.(*Error); !ok || e.Exit != ExitAuth {
		t.Fatalf("missing gh err = %v", err)
	}
}

// 评审第 1 轮 credential-dir-env-not-passed：凭据目录只来自 stdin 的 credentials_path，与 APA_HOME、cwd 无关；缺失是配置失败。
func TestRunStdinTakesCredentialsPathFromStdin(t *testing.T) {
	t.Setenv("APA_HOME", "/elsewhere")
	var got string
	api := &upstream{}
	var out, errb bytes.Buffer
	in := `{"source_id":"src_github","credentials_path":"/data/sources/src_github/gh","config":{"repos":["o/r"]}}`
	code := RunStdin(context.Background(), strings.NewReader(in), &out, &errb, func(p string) API { got = p; return api }, now)
	if code != ExitOK || got != "/data/sources/src_github/gh" || len(api.calls) == 0 {
		t.Fatalf("code=%d path=%q", code, got)
	}
	for _, bad := range []string{`{"source_id":"s","config":{"repos":["o/r"]}}`, `nope`} {
		out.Reset()
		code := RunStdin(context.Background(), strings.NewReader(bad), &out, &errb, func(string) API { t.Fatal("must not build API"); return nil }, now)
		if code != ExitAuth || out.Len() != 0 {
			t.Fatalf("%q: exit %d", bad, code)
		}
	}
}

func TestAuthRunsGHLoginInCredentialDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sources", "s", "gh")
	rec := filepath.Join(t.TempDir(), "rec")
	g := GH{Bin: fakeGH(t, `echo "$@ $GH_CONFIG_DIR" > `+rec), ConfigDir: dir}
	var sink bytes.Buffer
	if code := Auth(context.Background(), g, strings.NewReader(""), &sink, &sink); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(rec)
	if strings.TrimSpace(string(b)) != "auth login --insecure-storage "+dir {
		t.Errorf("gh invoked as %q", b)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("credential dir: %v %v", fi, err)
	}
	if code := Auth(context.Background(), GH{Bin: fakeGH(t, "exit 1"), ConfigDir: dir}, strings.NewReader(""), &sink, &sink); code != ExitAuth {
		t.Errorf("failed login exit %d", code)
	}
}

// 内核首次运行的真实 stdin：window.from 为空串（runner.stdinWindow 无 omitempty）。
func TestFirstRunWithKernelEmptyWindowFrom(t *testing.T) {
	api := &upstream{}
	in := `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"","window":{"from":"","to":"2026-11-03T00:00:00Z"},"run_id":"r"}`
	var out, errb bytes.Buffer
	if code := Run(context.Background(), strings.NewReader(in), &out, &errb, api, now); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(api.issueCalls()[0], "since=2026-11-03T00:00:00Z") {
		t.Errorf("empty from should start at window.to: %s", api.calls[0])
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var last map[string]string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last["cursor"] == "" {
		t.Fatalf("no cursor in last line %q", lines[len(lines)-1])
	}
}

func sha(s string) *string { return &s }

func agentsEvents(o output) []map[string]any {
	var evs []map[string]any
	for _, ev := range o.events {
		if ev["type"] == "repo.agents_md_changed" {
			evs = append(evs, ev)
		}
	}
	return evs
}

// §12.5：默认分支根目录 AGENTS.md 的 blob sha 变化（含新建、删除）才发事件；游标记住 sha，同一状态重放不再发。
func TestAgentsMDChangeEmitsOneEventPerChange(t *testing.T) {
	api := &upstream{}
	// 首次没有文件：只记下，不发
	o := pullOnce(t, api, repoCfg, "")
	if len(agentsEvents(o)) != 0 {
		t.Fatalf("absent file on first run emitted %v", o.ids())
	}
	// 新建
	api.agents = sha("aaa")
	o = pullOnce(t, api, repoCfg, o.cursor)
	evs := agentsEvents(o)
	if len(evs) != 1 || evs[0]["id"] != "repo-o-r-agents-md-aaa-"+fmt.Sprint(now().Unix()) || evs[0]["correlation_keys"].([]any)[0] != "o/r" ||
		evs[0]["link"] != "https://github.com/o/r/blob/main/AGENTS.md" || evs[0]["subject"] == "" {
		t.Fatalf("created: %v", o.events)
	}
	// 不变：不发；同一游标重放：输出相同
	same := pullOnce(t, api, repoCfg, o.cursor)
	if len(agentsEvents(same)) != 0 || same.cursor != o.cursor {
		t.Fatalf("unchanged emitted %v", same.ids())
	}
	// 修改
	api.agents = sha("bbb")
	o2 := pullOnce(t, api, repoCfg, o.cursor)
	if evs := agentsEvents(o2); len(evs) != 1 || evs[0]["id"] != "repo-o-r-agents-md-bbb-"+fmt.Sprint(now().Unix()) {
		t.Fatalf("changed: %v", o2.events)
	}
	if again := pullOnce(t, api, repoCfg, o.cursor); !slices.Equal(again.ids(), o2.ids()) {
		t.Errorf("replay differs: %v vs %v", again.ids(), o2.ids())
	}
	// 删除
	api.agents = nil
	o3 := pullOnce(t, api, repoCfg, o2.cursor)
	if evs := agentsEvents(o3); len(evs) != 1 || !strings.HasPrefix(evs[0]["id"].(string), "repo-o-r-agents-md-removed-") {
		t.Fatalf("removed: %v", o3.events)
	}
	if len(agentsEvents(pullOnce(t, api, repoCfg, o3.cursor))) != 0 {
		t.Error("removal emitted twice")
	}
}

// 首次就有文件：发一条（已有手写 AGENTS.md 的仓库首次检查落入漂移，§12.5）。
func TestAgentsMDExistingOnFirstRunEmits(t *testing.T) {
	api := &upstream{agents: sha("abc")}
	if evs := agentsEvents(pullOnce(t, api, repoCfg, "")); len(evs) != 1 {
		t.Fatalf("want one event, got %d", len(evs))
	}
}

// 读 AGENTS.md 失败（非 404）按取数失败处理：不输出、不推进游标；认证类仍是退出码 2。
func TestAgentsMDReadFailureIsNotMissingFile(t *testing.T) {
	api := &failContents{upstream: &upstream{}, err: &Error{Exit: ExitTemporary, Msg: "HTTP 502"}}
	in := `{"source_id":"s","config":{"repos":["o/r"]},"window":{"from":"2026-10-30T00:00:00Z"}}`
	var out, errb bytes.Buffer
	if code := Run(context.Background(), strings.NewReader(in), &out, &errb, api, now); code != ExitTemporary || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
	api.err = &Error{Exit: ExitAuth, Msg: "HTTP 403"}
	if code := Run(context.Background(), strings.NewReader(in), &out, &errb, api, now); code != ExitAuth {
		t.Fatalf("exit %d", code)
	}
}

type failContents struct {
	*upstream
	err error
}

func (f *failContents) Get(ctx context.Context, path string) ([]byte, error) {
	if strings.Contains(path, "/contents/") {
		return nil, f.err
	}
	return f.upstream.Get(ctx, path)
}

// gh 的 404 输出被标成 NotFound（仍是退出码 2），其他认证类错误不是。
func TestGHClassifiesNotFound(t *testing.T) {
	if e := classify("gh: Not Found (HTTP 404)"); !e.NotFound || e.Exit != ExitAuth {
		t.Errorf("404: %+v", e)
	}
	if e := classify("gh: Bad credentials (HTTP 401)"); e.NotFound || e.Exit != ExitAuth {
		t.Errorf("401: %+v", e)
	}
}

// sha 回到历史值（aaa→bbb→aaa、aaa→删除→aaa）是新的变化，id 不能与旧的相同，否则被内核去重而漏报；
// 同一游标、同一 window.to 重放 id 不变。每次运行用不同的 window.to，与内核按时钟逐次安排一致。
func TestAgentsMDIDsDistinctWhenShaReturnsToHistory(t *testing.T) {
	api := &upstream{}
	cursor := ""
	run := func(step int, a *string) output {
		api.agents = a
		in, _ := json.Marshal(map[string]any{"source_id": "s", "config": json.RawMessage(repoCfg), "cursor": cursor,
			"window": map[string]string{"from": at(0), "to": at(time.Duration(step) * time.Hour)}})
		var out bytes.Buffer
		if code := Run(context.Background(), bytes.NewReader(in), &out, &bytes.Buffer{}, api, now); code != ExitOK {
			t.Fatalf("exit %d", code)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		var last map[string]string
		_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
		prev := cursor
		cursor = last["cursor"]
		var ids []string
		for _, l := range lines[:len(lines)-1] {
			var ev map[string]any
			_ = json.Unmarshal([]byte(l), &ev)
			ids = append(ids, ev["id"].(string))
		}
		return output{cursor: prev, events: func() []map[string]any {
			var m []map[string]any
			for _, id := range ids {
				m = append(m, map[string]any{"id": id, "type": "repo.agents_md_changed"})
			}
			return m
		}()}
	}
	var ids []string
	for i, a := range []*string{sha("aaa"), sha("bbb"), sha("aaa"), nil, sha("aaa")} {
		o := run(i+1, a)
		if len(o.events) != 1 {
			t.Fatalf("step %d: %v", i, o.ids())
		}
		ids = append(ids, o.ids()...)
		if i == 4 { // 同一旧游标、同一 window.to 重放
			saved := cursor
			cursor = o.cursor
			if again := run(i+1, a); !slices.Equal(again.ids(), o.ids()) {
				t.Errorf("replay changed ids: %v vs %v", again.ids(), o.ids())
			}
			cursor = saved
		}
	}
	slices.Sort(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		t.Errorf("duplicate ids: %v", ids)
	}
}
