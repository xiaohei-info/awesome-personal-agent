package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveHome(t *testing.T) {
	if got, _ := ResolveHome(func(string) string { return "/data/apa" }, "/home/u"); got != "/data/apa" {
		t.Fatalf("APA_HOME ignored: %q", got)
	}
	if got, _ := ResolveHome(func(string) string { return "" }, "/home/u"); got != filepath.Join("/home/u", ".apa") {
		t.Fatalf("default home: %q", got)
	}
	if _, err := ResolveHome(func(string) string { return "rel/path" }, "/home/u"); err == nil {
		t.Fatal("relative APA_HOME must be rejected")
	}
	if _, err := ResolveHome(func(string) string { return "" }, ""); err == nil {
		t.Fatal("no APA_HOME and no user home must fail")
	}
}

func TestLayout(t *testing.T) {
	l := LayoutOf("/h")
	want := map[string]string{
		l.Config: "/h/config.yaml", l.Ledger: "/h/ledger.db", l.Blobs: "/h/blobs",
		l.Packs: "/h/packs", l.Sources: "/h/sources", l.Runs: "/h/runs",
		l.RunCredentials: "/h/run-credentials", l.Logs: "/h/logs",
		l.HostSources: "/h/sources.json", l.HostDeclarations: "/h/declarations",
		l.RunDir("r1"): "/h/runs/r1", l.SourceDir("s1"): "/h/sources/s1", l.PackDir("p"): "/h/packs/p",
		l.RunCredentialFile("r1"): "/h/run-credentials/r1",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got %q want %q", got, w)
		}
	}
}

func TestEnsureLayoutPermissions(t *testing.T) {
	l := LayoutOf(filepath.Join(t.TempDir(), "apa"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil { // 幂等
		t.Fatal(err)
	}
	for _, d := range []string{l.Blobs, l.Packs, l.Sources, l.Runs, l.Logs} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("missing dir %s: %v", d, err)
		}
	}
	if st, _ := os.Stat(l.RunCredentials); st.Mode().Perm() != 0o700 {
		t.Fatalf("run-credentials perm %v", st.Mode().Perm())
	}
}

func TestLoadDefaultsWhenMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Fatalf("got %+v", c)
	}
	if c.MaxSubprocesses != 4 || c.MaxShortSubprocesses != 2 {
		t.Fatal("quota defaults")
	}
	if c.IngestRetention != 30*24*time.Hour || c.RunRetention != 14*24*time.Hour {
		t.Fatal("retention defaults")
	}
	// 设计 §6.9：无人领取 30 分钟；失败退避 1、3、7、15、30 分钟。
	if c.DeliveryUnclaimedAfter != 30*time.Minute ||
		!reflect.DeepEqual(c.DeliveryBackoff, []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 15 * time.Minute, 30 * time.Minute}) {
		t.Fatalf("delivery defaults: %v %v", c.DeliveryUnclaimedAfter, c.DeliveryBackoff)
	}
	if c.ChainDepthThreshold != 8 { // §14.9
		t.Fatalf("chain depth threshold default = %d", c.ChainDepthThreshold)
	}
}

func TestLoadOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte(`# comment
listen: "0.0.0.0:9000"   # inline
max_subprocesses: 8
max_short_subprocesses: 1
max_source_output_mb: 5
ingest_retention_days: 7
run_retention_days: 3
delivery_unclaimed_after: 20s
delivery_backoff: 1s, 2s,3s
chain_depth_threshold: 2
guideline_draft_executor: wk.cli
unjudged_rejudge_days: 0
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || c.MaxSubprocesses != 8 || c.MaxShortSubprocesses != 1 || c.MaxSourceOutput != 5<<20 ||
		c.IngestRetention != 7*24*time.Hour || c.RunRetention != 3*24*time.Hour ||
		c.DeliveryUnclaimedAfter != 20*time.Second || !reflect.DeepEqual(c.DeliveryBackoff, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}) ||
		c.ChainDepthThreshold != 2 || c.GuidelineDraftExecutor != "wk.cli" || c.UnjudgedRejudgeWindow != 0 {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("max_subprocesses: 6\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxSubprocesses != 6 || c.MaxShortSubprocesses != 2 || c.Listen != Default().Listen {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "bogus: 1\n",
		"zero quota":       "max_subprocesses: 0\n",
		"negative quota":   "max_short_subprocesses: -1\n",
		"not a number":     "max_subprocesses: many\n",
		"zero retention":   "run_retention_days: 0\n",
		"empty listen":     "listen: \"\"\n",
		"bad listen":       "listen: nohostport\n",
		"duplicate key":    "max_subprocesses: 1\nmax_subprocesses: 2\n",
		"no colon":         "max_subprocesses 4\n",
		"nested":           "limits:\n  a: 1\n",
		"unterminated":     "listen: \"abc\n",
		"zero unclaimed":   "delivery_unclaimed_after: 0s\n",
		"unit missing":     "delivery_unclaimed_after: 30\n",
		"bad backoff":      "delivery_backoff: 1m,soon\n",
		"empty backoff":    "delivery_backoff: \n",
		"negative step":    "delivery_backoff: 1m,-3m\n",
		"zero chain":       "chain_depth_threshold: 0\n",
		"chain not int":    "chain_depth_threshold: deep\n",
		"empty executor":   "guideline_draft_executor:\n",
		"negative rejudge": "unjudged_rejudge_days: -1\n",
	}
	for name, body := range cases {
		p := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected error", name)
		} else if !strings.Contains(err.Error(), "config.yaml") {
			t.Errorf("%s: error lacks file name: %v", name, err)
		}
	}
}

func TestLoadUnreadableIsError(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil { // 目录不是文件
		t.Fatal("reading a directory must fail, not fall back to defaults")
	}
}

func TestOpenBootstrap(t *testing.T) {
	home := filepath.Join(t.TempDir(), "apa")
	env := func(k string) string {
		if k == "APA_HOME" {
			return home
		}
		return ""
	}
	r, err := Open(env, "/unused")
	if err != nil {
		t.Fatal(err)
	}
	if r.Layout.Home != home || r.Config.MaxSubprocesses != 4 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(r.Layout.Runs); err != nil {
		t.Fatal("Open must create layout")
	}
}

func TestPublicURLAndExposure(t *testing.T) {
	load := func(body string) (Config, error) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}
	cases := []struct {
		name, body, loadErr, exposeErr string
	}{
		{"default loopback needs no public_url", "", "", ""},
		{"localhost", "listen: localhost:7777\n", "", ""},
		{"ipv6 loopback", "listen: \"[::1]:7777\"\n", "", ""},
		{"all interfaces without public_url", "listen: 0.0.0.0:7777\n", "", "public_url"},
		{"empty host without public_url", "listen: :7777\n", "", "public_url"},
		{"named host without public_url", "listen: apa.example.com:7777\n", "", "public_url"},
		{"all interfaces with https", "listen: 0.0.0.0:7777\npublic_url: https://apa.example.com\n", "", ""},
		{"http public_url refused", "listen: 0.0.0.0:7777\npublic_url: http://apa.example.com\n", "https", ""},
		{"public_url without host", "public_url: https://\n", "https", ""},
	}
	for _, c := range cases {
		cfg, err := load(c.body)
		if c.loadErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.loadErr) {
				t.Errorf("%s: load err = %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		err = cfg.CheckExposure()
		if (c.exposeErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.exposeErr)) {
			t.Errorf("%s: CheckExposure = %v", c.name, err)
		}
	}
}
