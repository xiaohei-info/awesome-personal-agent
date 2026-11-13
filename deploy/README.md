# 部署样例

依据设计 §16。样例中的域名都是占位（`*.example.invalid`），使用前换成自己的。APA 不提供托管服务。

| 文件 | 用途 |
|---|---|
| `systemd/apa.service` | 把 `apa serve` 注册为系统服务 |
| `nginx/apa.conf.example`、`caddy/Caddyfile.example` | Web 与 API 全部经 HTTPS |
| `nginx/apa-ingest-only.conf.example`、`caddy/Caddyfile.ingest-only.example` | 只转发 `/ingest/`，其余 404 |
| `tunnel/cloudflared.yml.example` | 不开放入站端口，经 cloudflared 隧道暴露（Tailscale 等私有网络同理，指向 `127.0.0.1:7777`） |
| `container/Dockerfile` | 可选容器镜像 |

要点：
- `apa` 默认只监听回环（`config.yaml` 的 `listen`）。代理与 `apa` 同机时无需改动；容器用 `--network host` 运行（Linux），不要 `-p` 发布端口：经发布端口到达的请求源地址不是回环，apa 会拒绝登录。
- 代理必须保留 `Host`，并为 HTTPS 请求带上 `X-Forwarded-Proto: https`（apa 只信回环来源的该头）。Caddy 默认已处理。
- 验证：`go test -count=1 ./tests/deploy/...`
