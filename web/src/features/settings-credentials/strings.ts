import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 设置 › 接入钥匙：给 agent 的令牌与给扩展程序的密钥（§14.2）。明文只在签发时显示一次。
const zh = {
  ...common.zh,
  title: "接入钥匙",
  summary: (n: number) => `${n} 把在用`,
  lede: "给 agent 和扩展程序的钥匙。",
  groups: { tokens: "给 agent 的", "ext-keys": "给扩展程序的" },
  scope: "用途",
  ext: { channel: "消息渠道", export: "导出到文件夹", forward: "转发到外部系统" } as Record<string, string>,
  channelName: "渠道名",
  exportName: "名称（可选）",
  scopeLabel: (kind: string, name: string) => (name ? `${kind}：${name}` : kind),
  kind: "给谁",
  label: "备注",
  executor: "绑定的执行者",
  noExecutor: "（不绑定）",
  issue: "签发",
  none: "还没有",
  revoked: "已停用",
  revoke: "停用",
  secretOnce: "只显示这一次，请立即保存：",
  savedIt: "我已保存",
};

const en: typeof zh = {
  ...common.en,
  title: "Access keys",
  summary: (n) => `${n} in use`,
  lede: "Keys for agents and extensions.",
  groups: { tokens: "For agents", "ext-keys": "For extensions" },
  scope: "Purpose",
  ext: { channel: "Messaging channel", export: "Export to a folder", forward: "Forward to another system" },
  channelName: "Channel name",
  exportName: "Name (optional)",
  scopeLabel: (kind, name) => (name ? `${kind}: ${name}` : kind),
  kind: "For",
  label: "Note",
  executor: "Bound executor",
  noExecutor: "(not bound)",
  issue: "Issue",
  none: "None yet",
  revoked: "Revoked",
  revoke: "Revoke",
  secretOnce: "Shown only once. Save it now:",
  savedIt: "I've saved it",
};

export const strings: Dict<typeof zh> = { zh, en };
