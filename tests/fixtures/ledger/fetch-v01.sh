#!/usr/bin/env bash
# 取得「旧版本」主程序与宿主插件包，供升级/回退演练使用（验收文档 ACT-ENV-08 的 APA_V01_BIN / APA_V01_PLUGIN）。
#
#   用法：fetch-v01.sh <输出目录> [git 版本，缺省为结构版本 2 的冻结提交]
#   输出：向 stdout 打印两行 export；其余进度走 stderr。
#
# 为什么缺省不是 v0.1.1 标签：标签早于 sources_runner 的第 2 步迁移（未判定原因，#254），而账本结构版本 2 包含它，
# v0.1.1 的二进制会把结构版本 2 的账本当成「比自己新」而拒绝启动。结构版本 2 的二进制要从冻结提交构建。
# 只读使用本仓库的 git 对象；构建在临时目录里做，不改工作区。
set -euo pipefail

out=${1:?usage: fetch-v01.sh <out-dir> [rev]}
here=$(cd "$(dirname "$0")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
rev=${2:-$(sed -n 's/^var Frozen = map\[int\]string{2: "\([0-9a-f]*\)".*/\1/p' "$here/fixture.go")}
[ -n "$rev" ] || { echo "fetch-v01: cannot resolve the frozen commit of structure version 2" >&2; exit 1; }
commit=$(git -C "$root" rev-parse --verify --quiet "$rev^{commit}") || {
  echo "fetch-v01: $rev is not in this clone (shallow clone? run: git fetch --unshallow --tags)" >&2; exit 3; }

mkdir -p "$out"
out=$(cd "$out" && pwd)
src=$(mktemp -d)
trap 'rm -rf "$src"' EXIT
git -C "$root" archive "$commit" | tar -x -C "$src"
echo "fetch-v01: building $commit" >&2
(cd "$src" && CGO_ENABLED=0 go build -trimpath -o "$out/apa-$commit" ./cli/apa)
tar -C "$src/plugins" --exclude=tests -czf "$out/apa-hermes-plugin-$commit.tar.gz" hermes
echo "export APA_V01_BIN=$out/apa-$commit"
echo "export APA_V01_PLUGIN=$out/apa-hermes-plugin-$commit.tar.gz"
