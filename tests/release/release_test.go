// Package release 是首批发布收口（flow-release 的 ACT-REL-04、ACT-REL-05）：
// 在发布提交上跑红线检查、扫描仓库凭据与个人信息、证明通用能力不依赖 harness，并校验验收证据汇总。
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("找不到仓库根（go.mod）")
	}
}

func run(t *testing.T, root string, env []string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out), 0
}

// ACT-REL-05：发布提交上红线检查通过。
func TestRedlineOnReleaseCommit(t *testing.T) {
	root := repoRoot(t)
	out, code := run(t, root, os.Environ(), "go", "run", "./tools/redline", ".")
	if code != 0 || !strings.Contains(out, "redline: ok") {
		t.Fatalf("红线检查未通过（退出码 %d）:\n%s", code, out)
	}
}

// 凭据、个人信息、机器信息的检测规则。示例与测试数据一律用占位符，所以这里的规则不放过任何真实形态。
var leakRules = []struct {
	name string
	re   *regexp.Regexp
	ok   func(match string) bool // 返回 true 表示是允许的占位符
}{
	{"凭据", regexp.MustCompile(`sk-[A-Za-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|\b[0-9]{8,}:AA[A-Za-z0-9_-]{30,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|AKIA[0-9A-Z]{16}`), nil},
	{"邮箱", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`), placeholderEmail},
	{"本机路径", regexp.MustCompile(`/Users/[A-Za-z0-9._-]+|/home/[A-Za-z0-9._-]+|/Volumes/[A-Za-z0-9._-]+|[A-Za-z]:\\Users\\[A-Za-z0-9._-]+`), placeholderPath},
	{"内网地址", regexp.MustCompile(`\b(?:10(?:\.[0-9]{1,3}){3}|192\.168(?:\.[0-9]{1,3}){2}|172\.(?:1[6-9]|2[0-9]|3[01])(?:\.[0-9]{1,3}){2}|100\.(?:6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])(?:\.[0-9]{1,3}){2})\b`), placeholderAddr},
}

func placeholderEmail(m string) bool {
	domain := m[strings.LastIndex(m, "@")+1:]
	switch {
	case domain == "example.com", domain == "example.org", domain == "example.net", domain == "users.noreply.github.com":
		return true
	case strings.HasSuffix(domain, ".example"), strings.HasSuffix(domain, ".invalid"), strings.HasSuffix(domain, ".test"), strings.HasSuffix(domain, ".local"), domain == "localhost":
		return true
	}
	// SSH 用户与 iCalendar 事件 UID（uid-*@google.com，测试夹具）不是个人邮箱。
	return m == "git@github.com" || strings.HasPrefix(m, "uid-") && domain == "google.com"
}

// 测试数据里的通用账号名（不对应任何真实用户）。
var genericAccounts = map[string]bool{"user": true, "example": true, "apa": true, "me": true, "alice": true, "bob": true,
	"u": true, "x": true, "o": true, "runs": true, "workspace": true, "linuxbrew": true}

// 测试用的非路由黑洞地址与 CGNAT 段起点（用于暴露范围判定的边界值），不指向任何真实机器。
func placeholderAddr(m string) bool { return m == "10.255.255.1" || m == "100.64.0.0" }

func placeholderPath(m string) bool {
	return !strings.HasPrefix(m, "/Volumes/") && genericAccounts[m[strings.LastIndex(m, "/")+1:]]
}

// 本人决定公开的路径：.omac/（编排过程证据，worker 名里带机器名）与 tests/live/（live 测试既有主机名）。
// 只豁免"机器名或个人名"词表规则；凭据、邮箱、本机路径、内网地址对这两处照常检查。
var nameExemptPrefixes = []string{".omac/", "tests/live/"}

func nameExempt(rel string) bool {
	for _, p := range nameExemptPrefixes {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

func scan(rel string, data []byte, forbidden map[string]bool) []string {
	var hits []string
	text := string(data)
	for _, r := range leakRules {
		for _, m := range r.re.FindAllString(text, -1) {
			if r.ok == nil || !r.ok(m) {
				hits = append(hits, rel+": ["+r.name+"] "+redact(m))
			}
		}
	}
	words := wordRE.FindAllString(strings.ToLower(rel+"\n"+text), -1)
	if nameExempt(rel) {
		words = nil
	}
	for _, w := range words {
		if forbidden[wordHash(w)] {
			hits = append(hits, rel+": [机器名或个人名] 命中禁用词表中的一项")
			break
		}
	}
	return hits
}

var wordRE = regexp.MustCompile(`[a-z0-9]+`)

func wordHash(w string) string { h := sha256.Sum256([]byte(w)); return hex.EncodeToString(h[:]) }

// 个人姓名与机器名不能写进仓库，所以禁用词表只存小写词的 sha256。
func loadForbidden(t *testing.T, root string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "tests", "release", "forbidden.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			m[l] = true
		}
	}
	return m
}

// 命中内容本身可能就是泄漏，报告里只留头尾。
func redact(m string) string {
	if len(m) <= 8 {
		return m[:1] + "***"
	}
	return m[:4] + "…" + m[len(m)-2:]
}

// 检测器自检：真实形态必须命中，占位符必须放行；否则下面的全仓扫描通过也说明不了什么。
func TestLeakScannerRecognisesRealShapes(t *testing.T) {
	// 样例拼接生成，避免本文件自身成为命中项。
	bad := map[string]string{
		"凭据":   "token=" + "ghp_" + strings.Repeat("a1B2", 6),
		"邮箱":   "contact " + "someone" + "@" + "corp-mail" + ".com",
		"本机路径": "cd /Users/" + "somebody" + "/work",
		"内网地址": "http://" + "192.168" + ".1.20:8080",
	}
	for name, sample := range bad {
		if len(scan("sample", []byte(sample), nil)) == 0 {
			t.Errorf("检测器漏掉了 %s: %q", name, sample)
		}
	}
	for _, sample := range []string{"user@example.com", "git@github.com:org/repo.git", "/home/user/apa", "127.0.0.1:9090", "token=<token>"} {
		if hits := scan("sample", []byte(sample), nil); len(hits) != 0 {
			t.Errorf("占位符被误报 %q: %v", sample, hits)
		}
	}
	forb := map[string]bool{wordHash("alphahost"): true}
	if hits := scan("sample", []byte("ssh AlphaHost uptime"), forb); len(hits) != 1 {
		t.Errorf("禁用词应不区分大小写命中，实际 %v", hits)
	}
	if hits := scan("run-on-alphahost.sh", []byte("x"), forb); len(hits) != 1 {
		t.Errorf("文件名里的禁用词也应命中，实际 %v", hits)
	}
	// 豁免只作用于词表规则：.omac/ 与 tests/live/ 里的机器名放行，其余规则照常命中，其他路径的机器名照常命中。
	if hits := scan(".omac/state.yaml", []byte("worker alphahost"), forb); len(hits) != 0 {
		t.Errorf(".omac/ 的机器名应放行，实际 %v", hits)
	}
	if hits := scan("tests/live/alphahost/x.go", []byte("ssh alphahost"), forb); len(hits) != 0 {
		t.Errorf("tests/live/ 的机器名应放行，实际 %v", hits)
	}
	if hits := scan("docs/x.md", []byte("alphahost"), forb); len(hits) != 1 {
		t.Errorf("豁免前缀之外的机器名仍应命中，实际 %v", hits)
	}
	for name, sample := range bad {
		for _, rel := range []string{".omac/apa.state/_dag.yaml", "tests/live/x/evidence.txt"} {
			if len(scan(rel, []byte(sample), forb)) == 0 {
				t.Errorf("%s 下的 %s 仍应命中: %q", rel, name, sample)
			}
		}
	}
}

// ACT-REL-05：仓库（已跟踪文件）中没有凭据、个人姓名与邮箱、机器名、本机路径。
// 通用规则内置；个人姓名与机器名走哈希词表 forbidden.sha256。
func TestRepoHasNoCredentialsOrPersonalInfo(t *testing.T) {
	root := repoRoot(t)
	out, code := run(t, root, os.Environ(), "git", "ls-files", "-z")
	if code != 0 {
		t.Fatalf("git ls-files 退出码 %d: %s", code, out)
	}
	forbidden := loadForbidden(t, root)
	ev := loadEvidence(t, root)
	var hits []string
	scanned := 0
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" || skipFile(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue // 已跟踪但工作区删除（如构建产物目录），不属于发布内容
			}
			t.Fatal(err)
		}
		if isBinary(data) {
			continue
		}
		scanned++
		hits = append(hits, scan(rel, data, forbidden)...)
	}
	if scanned < 100 {
		t.Fatalf("只扫描了 %d 个文件，扫描范围不对", scanned)
	}
	// 已登记的未决发现（需要本人决定如何处置）可以容忍，但必须仍然存在，并使发布结论保持 blocked。
	used := make([]bool, len(ev.OpenFindings))
	var unexpected []string
	for _, h := range hits {
		covered := false
		for i, f := range ev.OpenFindings {
			if strings.HasPrefix(h, f.PathPrefix) && strings.Contains(h, "["+f.Rule+"]") {
				used[i], covered = true, true
			}
		}
		if !covered {
			unexpected = append(unexpected, h)
		}
	}
	for i, f := range ev.OpenFindings {
		if !used[i] {
			t.Errorf("open_findings 登记的 %s [%s] 已不再命中，请从 closeout-evidence.json 删除", f.PathPrefix, f.Rule)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Fatalf("发现 %d 处未登记的凭据或个人信息:\n%s", len(unexpected), strings.Join(unexpected, "\n"))
	}
}

func skipFile(rel string) bool {
	// 锁文件里的 integrity 哈希与包作者邮箱不是本仓库写入的内容；本文件的检测规则含样例形态。
	return strings.HasSuffix(rel, "package-lock.json") || rel == "go.sum" || rel == "tests/release/release_test.go"
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}

// harnessNames 是首版设计点名的 harness 可执行文件；与 tools/redline 的词表一致，只在测试里用来构造"未安装 harness"的主机。
var harnessNames = []string{"hermes", "claude", "codex", "dsh", "gemini", "opencode", "openclaw"}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// ---- 验收证据汇总 ----

type flowEvidence struct {
	ID       string   `json:"id"`
	Mode     string   `json:"mode"`   // hermetic | live
	Status   string   `json:"status"` // pass | fail | not-run | out-of-scope | partial
	Evidence []string `json:"evidence"`
	Note     string   `json:"note"`
}

type openFinding struct {
	PathPrefix    string `json:"path_prefix"`
	Rule          string `json:"rule"`
	Reason        string `json:"reason"`
	OwnerDecision string `json:"owner_decision"`
}

type evidenceIndex struct {
	BaseCommit   string         `json:"base_commit"`
	Verdict      string         `json:"verdict"` // releasable | blocked
	Flows        []flowEvidence `json:"flows"`
	OpenFindings []openFinding  `json:"open_findings"`
}

func loadEvidence(t *testing.T, root string) evidenceIndex {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "tests", "release", "closeout-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ev evidenceIndex
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		t.Fatalf("closeout-evidence.json: %v", err)
	}
	return ev
}

// 验收文档里的流程 id。
func acceptanceFlowIDs(t *testing.T, root string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".omac", "apa.acceptance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range regexp.MustCompile(`(?m)^  - id: (flow-[a-z0-9-]+)\s*$`).FindAllStringSubmatch(string(b), -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// 所有验收流程的证据汇总可追溯：与验收文档的流程一一对应，pass 必须指向存在的证据，
// fail / not-run 必须写明原因且不能被记为通过，发布结论由汇总推出而不是手填。
func TestCloseoutEvidenceIsTraceable(t *testing.T) {
	root := repoRoot(t)
	ev := loadEvidence(t, root)

	want := acceptanceFlowIDs(t, root)
	if len(want) < 50 {
		t.Fatalf("只从验收文档解析出 %d 个流程", len(want))
	}
	got := map[string]flowEvidence{}
	for _, f := range ev.Flows {
		if _, dup := got[f.ID]; dup {
			t.Errorf("流程 %s 重复", f.ID)
		}
		got[f.ID] = f
	}
	releasable := len(ev.OpenFindings) == 0
	for _, id := range want {
		f, ok := got[id]
		if !ok {
			t.Errorf("验收流程 %s 在证据汇总里没有记录", id)
			continue
		}
		delete(got, id)
		if f.Mode != "hermetic" && f.Mode != "live" {
			t.Errorf("%s: mode 必须是 hermetic 或 live，实际 %q", id, f.Mode)
		}
		switch f.Status {
		case "pass":
			if len(f.Evidence) == 0 {
				t.Errorf("%s: 记为 pass 必须给出证据", id)
			}
		case "fail", "not-run":
			releasable = false
			if strings.TrimSpace(f.Note) == "" {
				t.Errorf("%s: %s 必须写明原因", id, f.Status)
			}
		case "out-of-scope", "partial":
			// 本人决定的范围取舍：必须写明原因（partial 还要写明未执行的动作），不计缺口，不影响 verdict。
			if strings.TrimSpace(f.Note) == "" {
				t.Errorf("%s: %s 必须写明原因", id, f.Status)
			}
		default:
			t.Errorf("%s: status 必须是 pass、fail、not-run、out-of-scope 或 partial，实际 %q", id, f.Status)
		}
		for _, e := range f.Evidence {
			if strings.Contains(e, ":") { // 形如 "命令: go test ..." 的可复现命令，不是文件
				continue
			}
			if _, err := os.Stat(filepath.Join(root, e)); err != nil {
				t.Errorf("%s: 证据 %s 不存在", id, e)
			}
		}
	}
	for id := range got {
		t.Errorf("证据汇总里有验收文档没有的流程 %s", id)
	}
	for _, f := range ev.OpenFindings {
		if f.Reason == "" || f.OwnerDecision == "" {
			t.Errorf("open_findings %s 必须写明原因和待本人决定的事项", f.PathPrefix)
		}
	}
	wantVerdict := "blocked"
	if releasable {
		wantVerdict = "releasable"
	}
	if ev.Verdict != wantVerdict {
		t.Errorf("verdict 应由汇总推出为 %q，文件写的是 %q", wantVerdict, ev.Verdict)
	}
}
