package sources

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
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

// T1：发件人段必填且取值受限；可信时必须给格式提示。
func TestSenderDeclarationValidated(t *testing.T) {
	ok := []Sender{{Trust: TrustUnverified}, {Trust: TrustVerified, Hint: Text{"用户名", "username"}, Self: "identity", Types: map[string]string{"a.*": TrustNone}}}
	for _, s := range ok {
		if err := validSender(s); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := []Sender{{}, {Trust: "maybe"}, {Trust: TrustVerified}, {Trust: TrustUnverified, Self: "whoami"},
		{Trust: TrustUnverified, Types: map[string]string{"a.b": "trusted"}}, {Trust: TrustUnverified, Types: map[string]string{"[": TrustNone}}}
	for _, s := range bad {
		if err := validSender(s); err == nil {
			t.Errorf("%+v should be rejected", s)
		}
	}
}

// 按事件类型取上限：精确类型优先，其次最长的通配，都不中时用 trust。
func TestSenderForMatchesDeclaredTypes(t *testing.T) {
	ds, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	gh, _ := Find(ds, "webhook", "", "github")
	pim, _ := Find(ds, "script", "apple-pim-source", "")
	cases := []struct {
		d         Declaration
		typ, want string
	}{
		{gh, "pr.commented", TrustVerified}, {gh, "issue.opened", TrustVerified}, {gh, "pr.closed", TrustNone},
		{gh, "notification.mention", TrustNone}, {pim, "mail.received", TrustUnverified}, {pim, "reminder.due", TrustNone},
		{Declaration{Sender: Sender{Trust: TrustVerified, Types: map[string]string{"*": TrustNone, "a.*": TrustUnverified, "a.b": TrustVerified}}}, "a.c", TrustUnverified},
		{Declaration{Sender: Sender{Trust: TrustVerified, Types: map[string]string{"*": TrustNone, "a.*": TrustUnverified, "a.b": TrustVerified}}}, "a.b", TrustVerified},
	}
	for _, c := range cases {
		if got := c.d.SenderFor(c.typ); got != c.want {
			t.Errorf("%s %s: got %s, want %s", c.d.Program, c.typ, got, c.want)
		}
	}
}

// content 段可选，目前只有 attachments: fetch；写了别的值是构建缺陷。仓库自带的程序没有实测过取回（MEAS-RAW-ATTACH），都不声明。
func TestContentSectionAcceptsOnlyFetch(t *testing.T) {
	var d Declaration
	dec := yaml.NewDecoder(strings.NewReader("program: x\ncontent:\n  attachments: fetch\n"))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil || !d.FetchesAttachments() || validContent(d.Content) != nil {
		t.Fatalf("fetch declaration: %+v err=%v", d, err)
	}
	if (Declaration{}).FetchesAttachments() || validContent(Content{}) != nil {
		t.Fatal("no content section means no fetch")
	}
	if validContent(Content{Attachments: "auto"}) == nil {
		t.Fatal("unknown attachments value must be rejected")
	}
	ds, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range ds {
		if b.FetchesAttachments() {
			t.Errorf("%s declares fetch before MEAS-RAW-ATTACH confirms it", b.Program)
		}
	}
}
