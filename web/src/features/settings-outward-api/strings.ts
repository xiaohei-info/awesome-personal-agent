import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 设置 › 对外接口：外部程序来取数据、接转发用的密钥（§4.1、§14.2）。明文只在签发时显示一次。
// 与「提醒渠道」分清：这里是外部来向 APA 要；渠道是 APA 把提醒推给外部并接收回答。
const zh = {
  ...common.zh,
  title: "对外接口",
  summary: (n: number) => `${n} 把在用`,
  lede: "外部程序来取你的事项、或接收转发，用这里的钥匙。",
  hint: "程序由你自己写，APA 只提供接口和钥匙。",
  notChannel: "要接收提醒并回答的程序，不在这里，去「设置 › 提醒渠道」。",
  scope: "用途",
  types: { export: "导出到文件夹", forward: "转发到外部系统" } as Record<string, string>,
  exportName: "名称（可选）",
  scopeLabel: (kind: string, name: string) => (name ? `${kind}：${name}` : kind),
  label: "备注",
  issue: "签发",
  none: "还没有钥匙",
  revoked: "已停用",
  revoke: "停用",
  secretOnce: "只显示这一次，请立即保存：",
  savedIt: "我已保存",
};

const en: typeof zh = {
  ...common.en,
  title: "Outward API",
  summary: (n) => `${n} in use`,
  lede: "Keys for outside programs that pull your items or receive forwards.",
  hint: "You write the program; APA only provides the interface and the key.",
  notChannel: "A program that receives reminders and answers is not here: see Settings › Reminder channels.",
  scope: "Purpose",
  types: { export: "Export to a folder", forward: "Forward to another system" },
  exportName: "Name (optional)",
  scopeLabel: (kind, name) => (name ? `${kind}: ${name}` : kind),
  label: "Note",
  issue: "Issue",
  none: "No keys yet",
  revoked: "Revoked",
  revoke: "Revoke",
  secretOnce: "Shown only once. Save it now:",
  savedIt: "I've saved it",
};

export const strings: Dict<typeof zh> = { zh, en };
