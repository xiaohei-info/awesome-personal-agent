import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "后台运行",
  run: (what: string) => `后台运行：${what}`,
  access: (level: string) => `权限：${level}`,
  details: "详情",
  stats: "运行详情",
  session: (id: string) => `会话 ${id}`,
  rule: (name: string) => `由规则「${name}」发起`,
  depth: (n: number) => `连锁第 ${n} 层`,
  started: (at: string) => `开始 ${at}`,
  finished: (at: string) => `结束 ${at}`,
  took: (s: string) => `用时 ${s}`,
  exitCode: (n: number) => `退出码 ${n}`,
  timeoutSec: (n: number) => `时限 ${n} 秒`,
  state: "状态",
  all: "全部",
  reload: "重新加载",
  empty: "还没有后台运行",
};

const en: typeof zh = {
  ...common.en,
  title: "Background runs",
  run: (what) => `Background run: ${what}`,
  access: (level) => `Access: ${level}`,
  details: "Details",
  stats: "Run details",
  session: (id) => `Session ${id}`,
  rule: (name) => `Started by the rule "${name}"`,
  depth: (n) => `Chain level ${n}`,
  started: (at) => `Started ${at}`,
  finished: (at) => `Finished ${at}`,
  took: (s) => `Took ${s}`,
  exitCode: (n) => `Exit code ${n}`,
  timeoutSec: (n) => `Time limit ${n} s`,
  state: "State",
  all: "All",
  reload: "Reload",
  empty: "No background runs yet",
};

export const strings: Dict<typeof zh> = { zh, en };
