#!/usr/bin/env bash
# 用法：pkgs.sh <组名>   输出该组的 go 包参数（smoke、full-1..full-4）
#       pkgs.sh check    校验 full-* 恰好覆盖 tests/e2e 下全部包且不重复，smoke 是其子集
set -euo pipefail
cd "$(dirname "$0")"
if [ "${1:-}" = check ]; then
  all=$(cd ../../tests/e2e && ls -d */ | tr -d / | sort)
  full=$(cat full-*.txt | sort)
  [ "$all" = "$full" ] || { echo "full-*.txt 与 tests/e2e 不一致:"; diff <(echo "$all") <(echo "$full") || true; exit 1; }
  [ -z "$(comm -13 <(echo "$full") <(sort smoke.txt))" ] || { echo "smoke.txt 含未收录的包"; exit 1; }
  exit 0
fi
sed 's|^|./tests/e2e/|; s|$|/...|' "$1.txt"
