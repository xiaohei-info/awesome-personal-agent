// Package config 解析 APA_HOME、数据目录布局和 config.yaml（solution-design §16.2、§17.4、§7.7）。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	EnvHome    = "APA_HOME"
	day        = 24 * time.Hour
	configFile = "config.yaml"
)

// Config 是 config.yaml 的运行配置；缺省值见 Default。
type Config struct {
	Listen               string        // 监听地址 host:port
	MaxSubprocesses      int           // 长时运行（task、background）额度
	MaxShortSubprocesses int           // 短任务（source、transform、reader）额度
	MaxSourceOutput      int           // 来源、转换脚本单次 stdout 的总上限（字节）
	IngestRetention      time.Duration // Ingest 请求体保留期
	RunRetention         time.Duration // 运行目录保留期
	// 投递：无人领取多久转 fallback_web，以及失败后的逐次退避（§6.9）。
	DeliveryUnclaimedAfter time.Duration
	DeliveryBackoff        []time.Duration
	// ChainDepthThreshold 是链路深度阈值：超过只记录不派发（§14.9）。
	ChainDepthThreshold int
	// GuidelineDraftExecutor 是缺规范且本机无检出时受托起草的执行者 id（§12.6）；空表示未配置，起草事项转 needs_input。
	GuidelineDraftExecutor string
}

// Default 返回设计规定的默认值：额度 4/2，来源输出上限 32MB，Ingest 30 天，运行目录 14 天，
// 投递无人领取 30 分钟、失败退避 1/3/7/15/30 分钟，链路深度阈值 8。
// 监听地址设计未规定，默认只绑本机回环，跨设备访问需显式改配置。
func Default() Config {
	return Config{
		Listen:               "127.0.0.1:7777",
		MaxSubprocesses:      4,
		MaxShortSubprocesses: 2,
		MaxSourceOutput:      32 << 20,
		IngestRetention:      30 * day,
		RunRetention:         14 * day,

		DeliveryUnclaimedAfter: 30 * time.Minute,
		DeliveryBackoff:        []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 15 * time.Minute, 30 * time.Minute},
		ChainDepthThreshold:    8,
	}
}

// Layout 是 <APA_HOME> 下的固定布局。
type Layout struct {
	Home, Config, Ledger, Blobs, Packs, Sources, Runs, RunCredentials, Logs string
	// WebhookKeys 保管 HMAC 方案的 webhook 接收密钥明文（§14.2），每个来源一个文件。
	WebhookKeys string
	// HostSources 与 HostDeclarations 是本人在主机上写的登记（§14.12）；apa serve 启动时读取，不存在就跳过。
	HostSources, HostDeclarations string
}

func LayoutOf(home string) Layout {
	j := func(n string) string { return filepath.Join(home, n) }
	return Layout{Home: home, Config: j(configFile), Ledger: j("ledger.db"), Blobs: j("blobs"),
		Packs: j("packs"), Sources: j("sources"), Runs: j("runs"),
		RunCredentials: j("run-credentials"), Logs: j("logs"), WebhookKeys: j("webhook-keys"),
		HostSources: j("sources.json"), HostDeclarations: j("declarations")}
}

func (l Layout) PackDir(name string) string         { return filepath.Join(l.Packs, name) }
func (l Layout) SourceDir(id string) string         { return filepath.Join(l.Sources, id) }
func (l Layout) RunDir(id string) string            { return filepath.Join(l.Runs, id) }
func (l Layout) RunCredentialFile(id string) string { return filepath.Join(l.RunCredentials, id) }

// Ensure 幂等地建立目录；sources/、run-credentials/、webhook-keys/ 含凭据，权限 0700。
func (l Layout) Ensure() error {
	dirs := []struct {
		path string
		mode os.FileMode
	}{{l.Home, 0o755}, {l.Blobs, 0o755}, {l.Packs, 0o755}, {l.Runs, 0o755}, {l.Logs, 0o755},
		{l.Sources, 0o700}, {l.RunCredentials, 0o700}, {l.WebhookKeys, 0o700}}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if d.mode == 0o700 { // MkdirAll 受 umask 影响，凭据目录显式收紧
			if err := os.Chmod(d.path, d.mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// ResolveHome 取 APA_HOME，未设置时用 <userHome>/.apa。getenv 与 userHome 由调用方注入。
func ResolveHome(getenv func(string) string, userHome string) (string, error) {
	if h := getenv(EnvHome); h != "" {
		if !filepath.IsAbs(h) {
			return "", fmt.Errorf("%s 必须是绝对路径: %q", EnvHome, h)
		}
		return filepath.Clean(h), nil
	}
	if userHome == "" {
		return "", errors.New("未设置 APA_HOME 且无法确定用户主目录")
	}
	return filepath.Join(userHome, ".apa"), nil
}

// Runtime 是内核和 CLI 启动时需要的全部目录与配置。
type Runtime struct {
	Layout Layout
	Config Config
}

// Open 解析 APA_HOME、建立目录并读取 config.yaml。
func Open(getenv func(string) string, userHome string) (Runtime, error) {
	home, err := ResolveHome(getenv, userHome)
	if err != nil {
		return Runtime{}, err
	}
	l := LayoutOf(home)
	if err := l.Ensure(); err != nil {
		return Runtime{}, err
	}
	c, err := Load(l.Config)
	if err != nil {
		return Runtime{}, err
	}
	return Runtime{l, c}, nil
}

// Load 读取 config.yaml：文件不存在用默认值，其余读取或校验错误原样返回，不静默降级。
// 只支持顶层 `key: value` 标量（够用的 YAML 子集，未知键报错）。
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Config{}, err
	}
	seen := map[string]bool{}
	for i, raw := range strings.Split(string(data), "\n") {
		if err := apply(&c, seen, raw); err != nil {
			return Config{}, fmt.Errorf("%s:%d: %w", filepath.Base(path), i+1, err)
		}
	}
	return c, nil
}

func apply(c *Config, seen map[string]bool, raw string) error {
	line := strings.TrimRight(raw, " \t\r")
	if t := strings.TrimSpace(line); t == "" || t[0] == '#' {
		return nil
	}
	if line[0] == ' ' || line[0] == '\t' {
		return errors.New("不支持嵌套或缩进")
	}
	k, v, ok := strings.Cut(line, ":")
	if !ok {
		return errors.New("应为 key: value")
	}
	k = strings.TrimSpace(k)
	if seen[k] {
		return fmt.Errorf("重复的键 %q", k)
	}
	seen[k] = true
	v, err := scalar(v)
	if err != nil {
		return err
	}
	switch k {
	case "listen":
		if err := checkListen(v); err != nil {
			return err
		}
		c.Listen = v
	case "max_subprocesses":
		return positive(k, v, &c.MaxSubprocesses)
	case "max_short_subprocesses":
		return positive(k, v, &c.MaxShortSubprocesses)
	case "max_source_output_mb":
		var mb int
		if err := positive(k, v, &mb); err != nil {
			return err
		}
		c.MaxSourceOutput = mb << 20
	case "ingest_retention_days":
		return days(k, v, &c.IngestRetention)
	case "run_retention_days":
		return days(k, v, &c.RunRetention)
	case "delivery_unclaimed_after":
		return duration(k, v, &c.DeliveryUnclaimedAfter)
	case "delivery_backoff": // 逗号分隔的时长表，第 n 次失败后等第 n 项
		var bo []time.Duration
		for _, part := range strings.Split(v, ",") {
			var d time.Duration
			if err := duration(k, strings.TrimSpace(part), &d); err != nil {
				return err
			}
			bo = append(bo, d)
		}
		c.DeliveryBackoff = bo
	case "chain_depth_threshold":
		return positive(k, v, &c.ChainDepthThreshold)
	case "guideline_draft_executor":
		if v == "" {
			return fmt.Errorf("%s 不能为空；不配置就删掉这一行", k)
		}
		c.GuidelineDraftExecutor = v
	default:
		return fmt.Errorf("未知配置项 %q", k)
	}
	return nil
}

// scalar 去掉行内注释与成对引号。
func scalar(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v != "" && (v[0] == '"' || v[0] == '\'') {
		end := strings.IndexByte(v[1:], v[0])
		if end < 0 {
			return "", errors.New("引号未闭合")
		}
		if rest := strings.TrimSpace(v[end+2:]); rest != "" && rest[0] != '#' {
			return "", errors.New("引号后有多余内容")
		}
		return v[1 : end+1], nil
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), nil
}

func checkListen(v string) error {
	i := strings.LastIndexByte(v, ':')
	if i < 0 {
		return fmt.Errorf("listen 应为 host:port: %q", v)
	}
	if p, err := strconv.Atoi(v[i+1:]); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("listen 端口无效: %q", v)
	}
	return nil
}

func positive(k, v string, dst *int) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return fmt.Errorf("%s 必须是正整数: %q", k, v)
	}
	*dst = n
	return nil
}

func days(k, v string, dst *time.Duration) error {
	var n int
	if err := positive(k, v, &n); err != nil {
		return err
	}
	*dst = time.Duration(n) * day
	return nil
}

// duration 解析 Go 时长写法（30m、90s），必须为正。
func duration(k, v string, dst *time.Duration) error {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fmt.Errorf("%s 必须是正时长（如 30m、90s）: %q", k, v)
	}
	*dst = d
	return nil
}
