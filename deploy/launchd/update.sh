#!/bin/sh
# 在 macOS 上把 LaunchAgent 跑着的 apa 换成新二进制（设计 §16.5 升级不改写已有数据）。
# 用法：deploy/launchd/update.sh <新二进制>
#   环境变量（都有默认值）：APA_HOME（~/.apa）、APA_LAUNCHD_LABEL（info.xiaohei.apa）、
#   APA_INSTALL（~/.local/bin/apa）、APA_SIGN_ID（同 APA_LAUNCHD_LABEL）、APA_FORCE=1 跳过"有在跑的任务"检查。
# 步骤：重启前确认没有在跑或排队的运行 → 备份账本与旧二进制 → 记下账本序号与阶段分布 →
#       拷贝新二进制、以固定标识重签 → 原子替换 → kickstart → 核对 PID、版本与账本前后对比 → 打印回退命令。
#
# 为什么重签：本地 go build 出来的是 ad-hoc 签名，标识是 a.out、每次构建 cdhash 都变；macOS 的隐私授权（如"可移除宗卷"）
# 按签名记，换一次二进制授权就对不上，Agent 运行在外置盘上时会以退出码 126 失败。以固定标识和只认标识的指定要求重签后，
# 授权给一次就长期有效。
# 注意：重签的是拷贝出来的临时文件，再原子改名替换。不要对正在运行的那个文件就地重签：系统会判它签名失效并立即杀掉进程，
# 之后只能 kickstart 拉起。
set -eu

new=${1:?usage: update.sh <new apa binary>}
home=${APA_HOME:-$HOME/.apa}
label=${APA_LAUNCHD_LABEL:-info.xiaohei.apa}
install=${APA_INSTALL:-$HOME/.local/bin/apa}
sign_id=${APA_SIGN_ID:-$label}
ledger=$home/ledger.db
stamp=$(date +%Y%m%d-%H%M%S)
backup=$home/backup/update-$stamp

q() { sqlite3 -cmd ".timeout 5000" "file:$ledger?mode=ro" "$1"; }
snapshot() {
	echo "max_seq $(q 'select max(seq) from eventlog_event')"
	q "select 'phase', phase, count(*) from items group by phase order by phase"
}

[ -x "$new" ] || { echo "not executable: $new" >&2; exit 2; }
"$new" version >/dev/null

busy=$(q "select count(*) from outcall_run where state in ('running','queued')")
if [ "$busy" != 0 ] && [ "${APA_FORCE:-}" != 1 ]; then
	echo "$busy runs are running or queued; wait for them or set APA_FORCE=1" >&2
	q "select id, category, target, state from outcall_run where state in ('running','queued')" >&2
	exit 1
fi

mkdir -p "$backup"
chmod 700 "$backup"
sqlite3 -cmd ".timeout 5000" "$ledger" ".backup '$backup/ledger.db'"
[ ! -e "$install" ] || cp -p "$install" "$backup/apa"
snapshot >"$backup/before.txt"
old_pid=$(launchctl list | awk -v l="$label" '$3 == l { print $1 }')

tmp=$install.new
cp "$new" "$tmp"
chmod 755 "$tmp"
codesign -f -s - -i "$sign_id" -r="designated => identifier \"$sign_id\"" "$tmp"
mv -f "$tmp" "$install"
codesign -dv "$install" 2>&1 | grep '^Identifier='

launchctl kickstart -k "gui/$(id -u)/$label"
sleep 10
new_pid=$(launchctl list | awk -v l="$label" '$3 == l { print $1 }')
snapshot >"$backup/after.txt"

echo "pid $old_pid -> $new_pid"
"$install" version
echo "--- ledger before / after"
diff "$backup/before.txt" "$backup/after.txt" || true
old_max=$(awk '$1 == "max_seq" { print $2 }' "$backup/before.txt")
echo "--- events since seq $old_max"
q "select seq, type, object, coalesce(json_extract(actor, '\$.principal'), '') from eventlog_event where seq > ${old_max:-0} order by seq"
cat <<EOF
--- rollback
cp -p '$backup/apa' '$install' && launchctl kickstart -k gui/\$(id -u)/$label
(ledger snapshot: $backup/ledger.db; restoring it drops every event recorded after it)
EOF
