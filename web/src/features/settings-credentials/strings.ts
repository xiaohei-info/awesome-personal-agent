import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "凭据",
  lede: "harness、agent 与 ext 接入 APA 用的令牌和密钥。明文只在签发时显示一次。",
  groups: { tokens: "令牌", "ext-keys": "ext 密钥" },
  scope: "范围（scope）",
  kind: "类型",
  label: "标签",
  executor: "执行者",
  issue: "签发",
  none: "还没有凭据",
  revoked: "已吊销",
  revoke: "吊销",
  secretOnce: "明文只显示这一次，请立即保存：",
  savedIt: "我已保存",
};

const en: typeof zh = {
  ...common.en,
  title: "Credentials",
  lede: "Tokens and keys that harnesses, agents and ext programs use to reach APA. The plaintext is shown only once, when issued.",
  groups: { tokens: "Tokens", "ext-keys": "ext keys" },
  scope: "Scope",
  kind: "Kind",
  label: "Label",
  executor: "Executor",
  issue: "Issue",
  none: "No credentials yet",
  revoked: "revoked",
  revoke: "Revoke",
  secretOnce: "The plaintext is shown only this once. Save it now:",
  savedIt: "I have saved it",
};

export const strings: Dict<typeof zh> = { zh, en };
