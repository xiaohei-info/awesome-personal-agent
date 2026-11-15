import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 设置 › 宿主接入：宿主提交的接入请求（无头运行声明），以及高级里手写登记。执行者不在这里填（§6.17）。
const zh = {
  ...common.zh,
  title: "宿主接入",
  summary: (pending: number) => (pending ? `${pending} 个请求待批准` : "没有待批准的请求"),
  pending: "待批准的接入请求",
  pendingHint: "批准前不生效",
  noPending: "没有待批准的请求",
  wantsToRun: (host: string) => `${host} 想在本机运行：`,
  levels: (list: string) => `提供：${list}`,
  withExecutors: (n: number) => `附带执行者 ${n} 个（批准后进执行者名录）`,
  selfcheck: "另有一条自检命令",
  fullDeclaration: "查看完整声明",
  confirm: "允许它在本机运行这些命令",
  reject: "拒绝",
  badJSON: "声明不是合法的 JSON",
  commands: "已允许运行的命令",
  advanced: "高级：手写登记",
  declaration: "声明（JSON）",
  register: "登记",
  noCommands: "还没有允许运行的命令",
};

const en: typeof zh = {
  ...common.en,
  title: "Host access",
  summary: (pending) => (pending ? `${pending} requests to approve` : "Nothing to approve"),
  pending: "Access requests to approve",
  pendingHint: "Nothing takes effect before you approve",
  noPending: "No requests waiting",
  wantsToRun: (host) => `${host} wants to run on this machine: `,
  levels: (list) => `Offers: ${list}`,
  withExecutors: (n) => `${n} executors included (added to the executor list on approval)`,
  selfcheck: "Also a self-check command",
  fullDeclaration: "Full declaration",
  confirm: "Allow it to run these commands",
  reject: "Reject",
  badJSON: "The declaration is not valid JSON",
  commands: "Commands allowed to run",
  advanced: "Advanced: register by hand",
  declaration: "Declaration (JSON)",
  register: "Register",
  noCommands: "No commands allowed yet",
};

export const strings: Dict<typeof zh> = { zh, en };
