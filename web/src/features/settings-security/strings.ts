import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "登录安全",
  lede: "修改登录口令，查看并吊销在线会话。改口令后，除当前会话外的其他会话全部失效。",
  tooShort: (n: number) => `新口令至少 ${n} 个字符`,
  changePassword: "修改口令",
  changed: "口令已修改，其他会话已失效",
  current: "当前口令",
  next: "新口令",
  sessions: "在线会话",
  noSessions: "没有在线会话",
  unknownBrowser: "未知浏览器",
  lastUsed: (at: string) => `最近使用 ${at}`,
  currentSession: "当前会话",
  revoke: "吊销",
  revokeAll: "吊销全部会话",
};

const en: typeof zh = {
  ...common.en,
  title: "Sign-in security",
  lede: "Change the sign-in password, and review and revoke active sessions. After a password change, every session except this one is signed out.",
  tooShort: (n) => `The new password needs at least ${n} characters`,
  changePassword: "Change password",
  changed: "Password changed; other sessions are signed out",
  current: "Current password",
  next: "New password",
  sessions: "Active sessions",
  noSessions: "No active sessions",
  unknownBrowser: "Unknown browser",
  lastUsed: (at) => `last used ${at}`,
  currentSession: "This session",
  revoke: "Revoke",
  revokeAll: "Revoke all sessions",
};

export const strings: Dict<typeof zh> = { zh, en };
