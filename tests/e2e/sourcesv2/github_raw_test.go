//go:build e2e

package sourcesv2

import (
	"strings"
	"testing"
)

// TestFlowGithubRaw 覆盖 ACT-OAD-01：github-source 的正文全文进事件原文、总是带，作者、标签、状态、仓库与编号进 headers。
func TestFlowGithubRaw(t *testing.T) {
	k := setup(t)
	programs(t)
	const src = "src_gh_raw"
	k.step("ACT-OAD-01 超过 4000 字符的 issue 正文入账不截断，headers 带作者、标签、状态、仓库与编号", func(t *testing.T) {
		k.SetToolCase("gh-empty")
		k.addPull(src, "github-source", "github", map[string]any{"credentials-path": k.Home + "/sources/" + src},
			map[string]any{"repos": []string{"acc-org/acc-repo"}})
		k.cycle(src, 1)
		k.ghAt("gh-issue-long-body", hOpened)
		var ev SrcEvent
		k.until("issue #13 入账", func() bool {
			var ok bool
			ev, ok = k.event(src, "issue-acc-org-acc-repo#13-opened")
			return ok
		})
		r := k.fullEvent(ev)
		if n := len([]rune(r.Body)); n <= 4000 || !strings.HasSuffix(r.Body, "<!-- end-of-body-marker -->") || !strings.Contains(r.Body, "```sh\napa export --all\n```") {
			t.Errorf("body 应是未截断的 Markdown 原文: %d 个字符", n)
		}
		if r.Original.Text != "" {
			t.Errorf("Markdown 即原文，不应另给 original: %q", r.Original.MediaType)
		}
		labels, _ := r.Headers["Labels"].([]any)
		if r.Headers["Author"] != "reporter-c" || r.Headers["State"] != "open" || r.Headers["Repository"] != "acc-org/acc-repo" ||
			r.Headers["Number"] != "13" || len(labels) != 2 || labels[0] != "bug" || labels[1] != "export" {
			t.Errorf("headers: %v", r.Headers)
		}
		if r.Content.State != "full" {
			t.Errorf("取全了应是 full: %q", r.Content.State)
		}
	})
}
