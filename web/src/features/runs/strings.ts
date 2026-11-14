import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "运行记录",
  run: (id: string) => `运行 ${id}`,
  session: "会话：",
  access: "权限档位：",
  basis: "授权依据：",
  none: "无",
  basisRule: (rule: string, v: number) => `规则 ${rule} · 策略版本 ${v}`,
  details: "详情",
  stats: "执行统计",
  depth: (n: number) => `链路深度 ${n}`,
  started: (at: string) => `开始 ${at}`,
  finished: (at: string) => `结束 ${at}`,
  exitCode: (n: number) => `退出码 ${n}`,
  timeoutSec: (n: number) => `超时 ${n} 秒`,
  state: "状态",
  all: "全部",
  reload: "重新加载",
  empty: "没有运行记录",
};

const en: typeof zh = {
  ...common.en,
  title: "Runs",
  run: (id) => `Run ${id}`,
  session: "Session: ",
  access: "Access level: ",
  basis: "Authorization basis: ",
  none: "none",
  basisRule: (rule, v) => `rule ${rule} · policy version ${v}`,
  details: "Details",
  stats: "Execution stats",
  depth: (n) => `chain depth ${n}`,
  started: (at) => `started ${at}`,
  finished: (at) => `finished ${at}`,
  exitCode: (n) => `exit code ${n}`,
  timeoutSec: (n) => `timeout ${n} s`,
  state: "State",
  all: "All",
  reload: "Reload",
  empty: "No runs",
};

export const strings: Dict<typeof zh> = { zh, en };
