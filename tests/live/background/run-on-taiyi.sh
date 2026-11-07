#!/bin/sh
# 把本地工作区同步到 taiyi 的隔离临时目录，用 taiyi 上 /opt/apa-live 的真实 Hermes 跑 live 测试。
# 直接用 venv 里的 hermes：bin/hermes 包装脚本把 HERMES_HOME 固定为共享目录，这里每次运行要用自己的临时家目录。
# 用法: tests/live/background/run-on-taiyi.sh [go test 额外参数，如 -run 'TestFlowBgResume|TestBgResume' -v]（逐个加引号后传到远端）
# 只删除本次 mktemp 得到的 /tmp/apa-live.* 字面路径；不碰 /opt/apa-live、/root/.hermes、容器与服务。
set -eu
SRC=$(cd "$(dirname "$0")/../../.." && pwd)
ARGS=
for a in "$@"; do ARGS="$ARGS '$(printf %s "$a" | sed "s/'/'\\\\''/g")'"; done
T=$(ssh taiyi 'mktemp -d /tmp/apa-live.XXXXXX')
case "$T" in /tmp/apa-live.*) ;; *) echo "unexpected temp dir $T" >&2; exit 2;; esac
echo "taiyi workdir: $T" >&2
rsync -a --exclude .git --exclude node_modules --exclude web/dist "$SRC/" "taiyi:$T/src/"
ssh taiyi "T=$T; mkdir -p \$T/hermes-home \$T/apa-tmp &&
  cp /opt/apa-live/hermes-home/config.yaml /opt/apa-live/hermes-home/.env \$T/hermes-home/ && chmod 600 \$T/hermes-home/config.yaml \$T/hermes-home/.env &&
  cd \$T/src && npm --prefix web ci --no-audit --no-fund >/dev/null 2>&1 && npm --prefix web run build >/dev/null 2>&1 &&
  set -a && . /opt/apa-live/creds/live.env && set +a &&
  APA_LIVE_HERMES_BIN=/opt/apa-live/venv/bin/hermes APA_LIVE_HERMES_HOME=\$T/hermes-home TMPDIR=\$T/apa-tmp \
  go test -tags=live -count=1 -timeout=120m ./tests/live/background/... $ARGS" && rc=0 || rc=$?
echo "taiyi workdir kept for evidence: $T (exit $rc)" >&2
exit $rc
