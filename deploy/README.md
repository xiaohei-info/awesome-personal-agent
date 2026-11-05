# 部署样例

依据设计 §16。样例中的域名都是占位（`*.example.invalid`），使用前换成自己的。APA 不提供托管服务。

| 文件 | 用途 |
|---|---|
| `systemd/apa.service` | 把 `apa serve` 注册为系统服务 |
| `nginx/apa.conf.example`、`caddy/Caddyfile.example` | Web 与 API 全部经 HTTPS |
| `nginx/apa-ingest-only.conf.example`、`caddy/Caddyfile.ingest-only.example` | 只转发 `/ingest/`，其余 404 |
| `container/Dockerfile`、`container/config.yaml` | 可选容器镜像及其监听配置 |

要点：
- `apa` 默认只监听回环（`config.yaml` 的 `listen`）。代理与 `apa` 同机时无需改动；容器内需监听 `0.0.0.0:7777`（见 `container/config.yaml`），并只把端口发布到宿主回环。
- 代理必须保留 `Host`，并为 HTTPS 请求带上 `X-Forwarded-Proto: https`（apa 只信回环来源的该头）。Caddy 默认已处理。
- 验证：`go test -count=1 ./tests/deploy/...`
