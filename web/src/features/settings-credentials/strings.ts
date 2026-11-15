import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 设置 › 扩展程序：导出与转发用的密钥（§14.2）。明文只在签发时显示一次。
const zh = {
  ...common.zh,
  title: "扩展程序",
  summary: (n: number) => `${n} 把在用`,
  lede: "给导出、转发程序的钥匙。",
  scope: "用途",
  ext: { export: "导出到文件夹", forward: "转发到外部系统" } as Record<string, string>,
  exportName: "名称（可选）",
  scopeLabel: (kind: string, name: string) => (name ? `${kind}：${name}` : kind),
  label: "备注",
  issue: "签发",
  none: "还没有",
  revoked: "已停用",
  revoke: "停用",
  secretOnce: "只显示这一次，请立即保存：",
  savedIt: "我已保存",
};

const en: typeof zh = {
  ...common.en,
  title: "Extensions",
  summary: (n) => `${n} in use`,
  lede: "Keys for export and forwarding programs.",
  scope: "Purpose",
  ext: { export: "Export to a folder", forward: "Forward to another system" },
  exportName: "Name (optional)",
  scopeLabel: (kind, name) => (name ? `${kind}: ${name}` : kind),
  label: "Note",
  issue: "Issue",
  none: "None yet",
  revoked: "Revoked",
  revoke: "Revoke",
  secretOnce: "Shown only once. Save it now:",
  savedIt: "I've saved it",
};

export const strings: Dict<typeof zh> = { zh, en };
