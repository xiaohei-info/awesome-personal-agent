package sources

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 仓库自带的每个来源程序都有接入声明（hosted-reader 是托管文档读取器，不是来源）。
func TestEveryBundledProgramDeclaresAName(t *testing.T) {
	ds, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	progs, _ := filepath.Glob("*/cmd/*")
	if len(progs) == 0 {
		t.Fatal("no programs found")
	}
	for _, p := range progs {
		name := filepath.Base(p)
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() || name == "hosted-reader" {
			continue
		}
		if _, ok := Find(ds, "script", name, ""); !ok {
			t.Errorf("%s has no source.yaml declaration", name)
		}
	}
}

func TestNameForPicksByConfigAndFallsBackToDefault(t *testing.T) {
	ds, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ typ, command, kind, config, lang, want string }{
		{"script", "apple-pim-source", "mail", `{"kind":"mail","account":"iCloud"}`, "zh", "iCloud 邮件"},
		{"script", "/usr/local/bin/apple-pim-source --x", "mail", `{"kind":"mail","account":"QQ"}`, "en", "QQ Mail"}, // 路径与参数不影响
		{"script", "apple-pim-source", "calendar", `{"kind":"calendar"}`, "zh", "Apple 日历"},
		{"script", "apple-pim-source", "reminder", `{"kind":"reminders"}`, "zh", "提醒事项"},
		{"script", "apple-pim-source", "mail", `{"kind":"mail"}`, "zh", "Apple 邮件"}, // 占位取不到值
		{"script", "mail-himalaya", "mail", `{"account":"gmail"}`, "zh", "gmail 邮件"},
		{"script", "mail-himalaya", "mail", `{}`, "zh", "邮件"},
		{"script", "calendar", "calendar", `{"account":"me@example.com"}`, "zh", "Google 日历"},
		{"pull", "github-source", "github", `{"repos":["a/b"]}`, "zh", "GitHub"},
		{"webhook", "", "github", `{}`, "en", "GitHub"},
	}
	for _, c := range cases {
		d, ok := Find(ds, c.typ, c.command, c.kind)
		if !ok {
			t.Errorf("%s %q: no declaration", c.typ, c.command)
			continue
		}
		if got := d.NameFor(json.RawMessage(c.config), c.lang); got != c.want {
			t.Errorf("%s %s %s: got %q, want %q", c.command, c.config, c.lang, got, c.want)
		}
	}
	for _, c := range [][3]string{{"script", "/opt/my-feed.sh", ""}, {"webhook", "", "forge"}} {
		if _, ok := Find(ds, c[0], c[1], c[2]); ok {
			t.Errorf("%v should have no declaration", c)
		}
	}
}
