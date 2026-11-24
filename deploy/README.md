# 部署样例

依据设计 §16。样例中的域名都是占位（`*.example.invalid`），使用前换成自己的。APA 不提供托管服务。

| 文件 | 用途 |
|---|---|
| `systemd/apa.service` | 把 `apa serve` 注册为系统服务 |
| `nginx/apa.conf.example`、`caddy/Caddyfile.example` | Web 与 API 全部经 HTTPS |
| `nginx/apa-ingest-only.conf.example`、`caddy/Caddyfile.ingest-only.example` | 只转发 `/ingest/`，其余 404 |
| `tunnel/cloudflared.yml.example` | 不开放入站端口，经 cloudflared 隧道暴露（Tailscale 等私有网络同理，指向 `127.0.0.1:7777`） |
| `container/Dockerfile` | 可选容器镜像 |
| `launchd/update.sh` | macOS 上以 LaunchAgent 运行时换新二进制：重启前确认没有在跑的运行、备份账本与旧二进制、拷贝后以固定标识重签、kickstart、对比账本序号与阶段分布、打印回退命令；每次在 `<APA_HOME>/backup/update-<时刻>/` 留账本与旧二进制，只保留最近 10 份（脚本内常量 `keep_backups`），更早的列出体积后删除 |

要点：
- `apa` 默认只监听回环（`config.yaml` 的 `listen`）。代理与 `apa` 同机时无需改动；容器用 `--network host` 运行（Linux），不要 `-p` 发布端口：经发布端口到达的请求源地址不是回环，apa 会拒绝登录。
- 代理必须保留 `Host`，并为 HTTPS 请求带上 `X-Forwarded-Proto: https`（apa 只信回环来源的该头）。Caddy 默认已处理。
- macOS 本机部署必须重签：本地构建的二进制是 ad-hoc 签名（标识 `a.out`，每次构建都变），系统的隐私授权（如"可移除宗卷"）按签名记，换一次二进制就失效，Agent 装在外置盘上时运行会以退出码 126 失败。`launchd/update.sh` 在拷贝新二进制之后、替换之前执行 `codesign -f -s - -i <标识> -r='designated => identifier "<标识>"'`，授权给一次即长期有效。**不要对正在运行的那个文件就地重签**：系统会判它签名失效并立即杀掉进程，之后要 `launchctl kickstart -k gui/$(id -u)/<标识>` 才能拉起；脚本重签的是临时拷贝，再原子改名。
- 验证：`go test -count=1 ./tests/deploy/...`
