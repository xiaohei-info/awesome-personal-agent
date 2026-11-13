package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/backup"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ---- 运行配置 ----

func configCheck(l config.Layout) (kdoctor.Check, config.Config) {
	c := kdoctor.Check{Name: kdoctor.Config, Status: kdoctor.OK}
	cfg, err := config.Load(l.Config)
	if err != nil {
		c.Status, c.Why, c.Next = kdoctor.Error, "config.yaml cannot be parsed: "+err.Error(), "fix "+l.Config
		return c, cfg
	}
	if cfg.PublicURL == "" && cfg.CheckExposure() != nil {
		c.Status = kdoctor.Error
		c.Why = fmt.Sprintf("listen %s is not a local address and no https public_url is configured; apa serve refuses to start (§14.13)", cfg.Listen)
		c.Next = "set `public_url: https://<your domain>` in " + l.Config + ", or listen on 127.0.0.1"
		return c, cfg
	}
	if err := hostFiles(l); err != nil {
		c.Status, c.Why, c.Next = kdoctor.Error, err.Error(), "fix the host configuration file named above"
		return c, cfg
	}
	c.Why, c.Next = fmt.Sprintf("config.yaml and the host configuration files parse; listening on %s", cfg.Listen), "nothing to do"
	return c, cfg
}

// hostFiles 校验 sources.json 与 declarations/*.json 能被 apa serve 读入（不存在就跳过）。
func hostFiles(l config.Layout) error {
	if data, err := os.ReadFile(l.HostSources); err == nil {
		var f struct {
			Sources []registry.SourceInput `json:"sources"`
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return fmt.Errorf("sources.json cannot be parsed: %v", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sources.json cannot be read: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(l.HostDeclarations, "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil {
			_, err = executors.ParseDeclaration(data)
		}
		if err != nil {
			return fmt.Errorf("declarations/%s cannot be parsed: %v", filepath.Base(f), err)
		}
	}
	return nil
}

// ---- 数据目录与版本 ----

func dataDirCheck(l config.Layout, in Input, led *ledger, openErr error) kdoctor.Check {
	c := kdoctor.Check{Name: kdoctor.DataDir, Status: kdoctor.OK, Data: map[string]any{}}
	fail := func(s kdoctor.Status, why, next string) kdoctor.Check {
		c.Status, c.Why, c.Next = s, why, next
		return c
	}
	st, err := os.Stat(l.Home)
	if err != nil || !st.IsDir() {
		return fail(kdoctor.Error, "the data directory does not exist", "run `apa init`, or set APA_HOME to the data directory")
	}
	var bad []string
	if st.Mode().Perm()&0o022 != 0 {
		bad = append(bad, fmt.Sprintf("the data directory is writable by other users (mode %04o)", st.Mode().Perm()))
	}
	for _, d := range []string{l.Sources, l.WebhookKeys, l.RunCredentials} { // 含凭据的目录
		if s, err := os.Stat(d); err == nil && s.Mode().Perm()&0o077 != 0 {
			bad = append(bad, fmt.Sprintf("%s holds credentials but is accessible to other users (mode %04o)", filepath.Base(d), s.Mode().Perm()))
		}
	}
	if s, err := os.Stat(l.Ledger); err == nil && s.Mode().Perm()&0o022 != 0 {
		bad = append(bad, fmt.Sprintf("ledger.db is writable by other users (mode %04o)", s.Mode().Perm()))
	}
	if len(bad) > 0 {
		return fail(kdoctor.Error, "permissions: "+strings.Join(bad, "; "), "run `chmod 700 "+l.Home+"` and keep credential directories at 0700")
	}
	target := in.Reg.SchemaLedger()
	c.Data["schema_binary"] = target
	switch {
	case openErr != nil && errors.Is(openErr, os.ErrNotExist):
		return fail(kdoctor.Error, "there is no ledger.db in the data directory", "run `apa init`, or restore a backup with `apa backup restore`")
	case openErr != nil:
		return fail(kdoctor.Error, "the ledger cannot be read: "+openErr.Error(), "check the file permissions and the disk, or restore a backup")
	case led.infoErr != nil && errors.Is(led.infoErr, store.ErrSchemaTooNew):
		return fail(kdoctor.Error, fmt.Sprintf("the ledger was written by a newer version than this binary (ledger structure up to %d)", target), "use the binary that matches the data directory; never run an older binary on newer data")
	case led.infoErr != nil:
		return fail(kdoctor.Error, "the ledger is damaged or not a ledger: "+led.infoErr.Error(), "restore the latest backup with `apa backup restore`")
	}
	c.Data["schema_ledger"] = led.info.Ledger
	last, ok := lastBackup(l)
	if ok {
		c.Data["last_backup_at"] = ts(last)
	}
	when := "no backup found under backups/"
	if ok {
		when = "last backup " + ts(last)
	}
	switch {
	case led.info.Ledger < target:
		return fail(kdoctor.Warning, fmt.Sprintf("the ledger structure is %d, this binary expects %d; it migrates when apa serve starts (a pre-upgrade snapshot is taken first); %s", led.info.Ledger, target, when),
			"run `apa doctor --upgrade-check`, then start apa serve")
	case led.info.Ledger > target:
		return fail(kdoctor.Error, fmt.Sprintf("the ledger structure %d is newer than this binary (%d)", led.info.Ledger, target), "use the matching binary")
	case !ok:
		return fail(kdoctor.Warning, fmt.Sprintf("permissions fine, ledger readable, structure %d matches the binary; %s", target, when), "run `apa backup create`")
	}
	return fail(kdoctor.OK, fmt.Sprintf("permissions fine, ledger readable, structure %d matches the binary; %s", target, when), "nothing to do")
}

func lastBackup(l config.Layout) (time.Time, bool) {
	ents, _ := os.ReadDir(filepath.Join(l.Home, "backups"))
	var best time.Time
	for _, e := range ents {
		data, err := os.ReadFile(filepath.Join(l.Home, "backups", e.Name(), backup.ManifestFile))
		if err != nil {
			continue
		}
		var m backup.Manifest
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil && t.After(best) {
			best = t
		}
	}
	return best, !best.IsZero()
}

// ---- 宿主插件与重复提醒（附录 A.1） ----

type selfcheckOut struct {
	PluginVersion      string `json:"plugin_version"`
	ReportsSessionKind bool   `json:"reports_session_kind"`
	Hooks              []struct {
		Name      string `json:"name"`
		Installed bool   `json:"installed"`
	} `json:"hooks"`
	Overlaps []struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Detail string `json:"detail"`
	} `json:"overlaps"`
}

const selfcheckTimeout = 5 * time.Second

// runSelfcheck 在 APA 主机上以当前账号执行插件声明的自检命令（不经外呼调用器，附录 A.2）。
func runSelfcheck(ctx context.Context, argv, env []string) (out selfcheckOut, err error) {
	ctx, cancel := context.WithTimeout(ctx, selfcheckTimeout)
	defer cancel()
	cmd := bounded(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &limitWriter{w: &buf, n: 1 << 20}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out, fmt.Errorf("it did not finish within %s", selfcheckTimeout)
		}
		return out, fmt.Errorf("it could not run or exited with an error")
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return out, errors.New("its output is not the JSON of appendix A.1")
	}
	return out, nil
}

// bounded 让时限真正生效：命令放进自己的进程组，到时整组杀掉；管道被孙进程占着时 1 秒后也放弃等待。
func bounded(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("output too large")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

func hostChecks(ctx context.Context, in Input, f kdoctor.Facts) (host, overlaps kdoctor.Check) {
	host = kdoctor.Check{Name: kdoctor.HostPlugin, Status: kdoctor.OK}
	overlaps = kdoctor.Check{Name: kdoctor.Overlaps, Status: kdoctor.OK}
	env := in.Env
	if env == nil {
		env = os.Environ()
	}
	ran := 0
	for _, rec := range f.Declarations {
		d := kdoctor.Detail{Subject: rec.Harness, Status: kdoctor.OK, Data: map[string]any{}}
		var why, next []string
		if at, ok := f.Seen[kdoctor.SubjectHarness+rec.Harness]; ok {
			d.Data["last_call_at"] = ts(at)
		} else {
			d.Data["last_call_at"] = "never"
		}
		if h := rec.Declaration.Headless; h != nil && !kdoctor.CommandExists(h.Command[0]) {
			d.Status = kdoctor.Error
			why = append(why, "the confirmed headless declaration is not executable: its command does not exist on this host")
			next = append(next, "fix the headless command in the declaration and confirm it again in Web")
		}
		if sc := rec.Declaration.Selfcheck; sc != nil {
			out, err := runSelfcheck(ctx, sc.Command, env)
			if err != nil {
				d.Status = kdoctor.Worst(d.Status, kdoctor.Warning)
				why = append(why, "the plugin selfcheck failed: "+err.Error())
				next = append(next, "check the selfcheck.command of the declaration")
			} else {
				ran++
				d.Data["plugin_version"] = out.PluginVersion
				if !out.ReportsSessionKind {
					d.Status = kdoctor.Worst(d.Status, kdoctor.Warning)
					why = append(why, "the plugin does not report the session type, so agent answers on the owner's own words are rejected (exit 23, §14.5)")
					next = append(next, "upgrade the plugin together with the kernel")
				}
				for _, h := range out.Hooks {
					if !h.Installed {
						d.Status = kdoctor.Worst(d.Status, kdoctor.Warning)
						why = append(why, "hook "+h.Name+" is not installed")
						next = append(next, "install the plugin hook "+h.Name)
					}
				}
				for _, o := range out.Overlaps {
					overlaps.Add(kdoctor.Detail{Subject: rec.Harness + ":" + o.Kind + ":" + o.Name, Status: kdoctor.Warning,
						Why:  fmt.Sprintf("the host side has a %s that overlaps with APA: %s %s", o.Kind, o.Name, o.Detail),
						Next: "decide on the host side whether to keep it; APA does not change the host configuration"})
				}
			}
		}
		if len(why) == 0 {
			d.Why = "declaration confirmed and executable; plugin reports its session type"
			if rec.Declaration.Selfcheck == nil {
				d.Why = "declaration confirmed and executable; no selfcheck.command is declared"
			}
		} else {
			d.Why, d.Next = strings.Join(why, "; "), strings.Join(next, "; ")
		}
		host.Add(d)
	}
	switch {
	case len(f.Declarations) == 0:
		host.Why, host.Next = "no host harness has a confirmed declaration (tiers 1 and 2 do not need one)", "install the host plugin and confirm its declaration in Web to reach tier 3"
	case host.Status == kdoctor.OK:
		host.Why, host.Next = "the confirmed declarations are executable and plugins report their session type", "nothing to do"
	default:
		host.Why, host.Next = "a host plugin needs attention; see details", "fix what the details name"
	}
	switch {
	case ran == 0:
		overlaps.Why, overlaps.Next = "skipped: no plugin declares selfcheck.command (appendix A.1)", "nothing to do"
	case overlaps.Status == kdoctor.OK:
		overlaps.Why, overlaps.Next = "no overlapping injection hook or schedule was reported", "nothing to do"
	default:
		overlaps.Why, overlaps.Next = "the host side has injections or schedules that overlap with APA; listed only, nothing was changed", "remove the duplicates on the host side"
	}
	sort.Slice(overlaps.Details, func(i, j int) bool { return overlaps.Details[i].Subject < overlaps.Details[j].Subject })
	return host, overlaps
}

// ---- 工具适配来源（§9.7） ----

var toolKinds = map[string]bool{"mail": true, "calendar": true, "reminder": true}

const probeTimeout = 5 * time.Second

var envKeep = []string{"PATH=", "HOME=", "XDG_"}

// probeTools 对工具适配来源（source_type 为 mail、calendar、reminder）运行一次已登记的适配器，只看退出码与机制性的报错类别：
// 窗口起点取现在，不拉任何历史，输出丢弃；适配器在这里先检查工具是否安装、版本是否在声明范围内（§9.7）。
func probeTools(ctx context.Context, in Input, f kdoctor.Facts, c *kdoctor.Check) {
	for _, s := range f.Sources {
		if !s.Enabled || !toolKinds[s.SourceKind] || s.Type == domain.SourceWebhook || !kdoctor.CommandExists(s.Command) {
			continue
		}
		d, ok := c.Find(s.ID)
		if !ok {
			continue
		}
		problem, next := probeOne(ctx, in, s)
		d.Data["tool"] = "ok"
		if problem == "" {
			continue
		}
		d.Data["tool"] = "problem"
		d.Status = kdoctor.Worst(d.Status, kdoctor.Warning)
		if strings.HasPrefix(d.Why, "command found, ") { // 原来是"一切正常"的说明，换成问题本身
			d.Why = problem
		} else {
			d.Why += "; " + problem
		}
		d.Next = strings.Trim(d.Next+"; "+next, "; ")
		c.Status = kdoctor.Worst(c.Status, d.Status)
		if c.Why == "" || strings.Contains(c.Why, "commands exist") {
			c.Why, c.Next = "some sources need attention; see details", "fix the registration or choose the outbound scope on the source page"
		}
	}
}

// restoredSecrets：`apa backup restore` 恢复的是 --no-secrets 备份时，备份没带的来源凭据与 webhook 接收密钥要补齐
// （重新认证来源、轮换接收密钥）。记录里列的条目在数据目录里仍然缺失就在来源检查里给警告，补齐后自然消失。
func restoredSecrets(l config.Layout, c *kdoctor.Check) {
	n, ok := backup.ReadNotice(l.Home)
	if !ok {
		return
	}
	for _, w := range n.Withheld {
		kind, id, _ := strings.Cut(w, "/")
		var why, next string
		switch kind {
		case "sources":
			if ents, _ := os.ReadDir(filepath.Join(l.Sources, id)); len(ents) > 0 {
				continue
			}
			why, next = "restored from a --no-secrets backup: the source credentials are missing", "re-authenticate the source (`apa source auth "+id+"` or the tool's own login)"
		case "webhook-keys":
			if _, err := os.Stat(filepath.Join(l.WebhookKeys, id)); err == nil {
				continue
			}
			why, next = "restored from a --no-secrets backup: the webhook receive key is missing, the old key is rejected", "rotate the webhook key of this source and give the new key to the sender"
		default:
			continue
		}
		d, found := c.Find(id)
		if !found {
			c.Add(kdoctor.Detail{Subject: id, Status: kdoctor.Warning, Why: why, Next: next})
			continue
		}
		d.Status = kdoctor.Worst(d.Status, kdoctor.Warning)
		d.Why = strings.Trim(d.Why+"; "+why, "; ")
		d.Next = strings.Trim(d.Next+"; "+next, "; ")
		c.Status = kdoctor.Worst(c.Status, d.Status)
	}
	if c.Status != kdoctor.OK && strings.Contains(c.Why, "commands exist") {
		c.Why, c.Next = "some sources need attention; see details", "fix the registration or choose the outbound scope on the source page"
	}
	if c.Status != kdoctor.OK && c.Why == "no source is registered (optional)" {
		c.Why, c.Next = "restored secrets are missing; see details", "re-authenticate sources and rotate webhook keys"
	}
}

func probeOne(ctx context.Context, in Input, s domain.Source) (problem, next string) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	argv := strings.Fields(s.Command)
	cfg := s.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	now := in.Now.UTC().Format(time.RFC3339)
	stdin, _ := json.Marshal(map[string]any{"source_id": s.ID, "config": cfg, "cursor": "", "run_id": "doctor-probe",
		"credentials_path": s.CredentialsPath, "window": map[string]string{"from": now, "to": now}})
	cmd := bounded(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &limitWriter{w: &errOut, n: 64 << 10}
	if dir := filepath.Join(in.Home, "sources", s.ID); dirExists(dir) {
		cmd.Dir = dir
	}
	for _, kv := range os.Environ() {
		for _, k := range envKeep {
			if strings.HasPrefix(kv, k) {
				cmd.Env = append(cmd.Env, kv)
			}
		}
	}
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return "", ""
	case ctx.Err() != nil:
		return "the adapter did not finish the probe in time", "check the tool and the network, then run apa doctor again"
	case errors.As(err, &ee) && ee.ExitCode() == 2:
		msg := errOut.String()
		switch {
		case strings.Contains(msg, "声明范围") || strings.Contains(strings.ToLower(msg), "version"):
			return "the dependent tool's version is not in the range the adapter declares (§9.7)", "install a supported version of the tool (appendix D lists the ranges)"
		case strings.Contains(msg, "未找到") || strings.Contains(strings.ToLower(msg), "not found"):
			return "the dependent tool is not installed", "install the tool the adapter wraps (appendix D)"
		}
		return "the adapter reports a configuration or authentication problem (exit 2, §9.7)", "authenticate the tool with its own command and check the source's config"
	}
	return "the adapter probe failed temporarily (exit status or start failure)", "run apa doctor again; if it persists check the tool and the network"
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
