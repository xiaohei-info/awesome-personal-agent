//go:build live

package guideline

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// driftItems 是仓库的 AGENTS.md 漂移事项（needs_input，标题或正文含仓库地址）。
func (f *fx) driftItems(repo string) []string {
	f.t.Helper()
	var ids []string
	for _, it := range jsonOf[struct {
		Items []map[string]any `json:"items"`
	}](f.t, f.webOK("GET", "/items?phase=needs_input&limit=500", "", 200)).Items {
		b, _ := json.Marshal(it)
		if strings.Contains(string(b), "AGENTS.md") && strings.Contains(string(b), repo) {
			ids = append(ids, str(it, "id"))
		}
	}
	return ids
}

// githubSource 登记随发行附带的 github-source（拉取 repo），凭据只放在来源自己的目录（取自本机 gh，经 stdin 交接）。
func (f *fx) githubSource(repoURL string) {
	t := f.t
	repo := strings.TrimSuffix(strings.TrimPrefix(repoURL, "https://github.com/"), ".git")
	tok, err := exec.Command("gh", "auth", "token").Output()
	token := strings.TrimSpace(string(tok))
	if err != nil || token == "" {
		t.Fatal("取不到本机 gh token")
	}
	cred := filepath.Join(f.Home, "sources", "src_gh")
	must(t, os.MkdirAll(cred, 0o700))
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "GH_TOKEN" && k != "GITHUB_TOKEN" && k != "GH_CONFIG_DIR" {
			env = append(env, kv)
		}
	}
	login := exec.Command("gh", "auth", "login", "--with-token", "--insecure-storage")
	login.Env = append(env, "GH_CONFIG_DIR="+cred)
	login.Stdin = strings.NewReader(token + "\n")
	if out, err := login.CombinedOutput(); err != nil {
		t.Fatalf("来源目录认证失败: %v %s", err, f.redact(string(out)))
	}
	cfg, _ := json.Marshal(map[string]any{"repos": []string{repo}})
	r := f.Apa(f.DEL, "", "source", "add", "--id", "src_gh", "--type", "pull", "--command", "github-source", "--schedule", "5s",
		"--timeout", "60", "--source-type", "github", "--credentials-path", cred, "--dedupe-domain", "github", "--config", string(cfg), "--evidence", "登记 GitHub 拉取程序")
	if r.Code != 0 {
		t.Fatalf("source add: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
	f.waitFor("GitHub 来源首次拉取成功", 90e9, func() bool {
		s := jsonOf[struct {
			Cursor string `json:"cursor"`
			Stats  struct {
				LastError string `json:"last_error"`
			} `json:"stats"`
		}](t, f.apa(f.OBS, 0, "source", "show", "src_gh").Stdout)
		return s.Cursor != "" && s.Stats.LastError == ""
	})
}

// buildGithubSource 把随发行附带的拉取程序构建到 PATH 上（设计 §9.1 按裸名登记）；须在 apa serve 启动之前调用。
func buildGithubSource(t *testing.T) {
	t.Helper()
	progDir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(progDir, "github-source"), "./sources/github/cmd/github-source")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 github-source: %v\n%s", err, out)
	}
	t.Setenv("PATH", progDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
