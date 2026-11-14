import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "登录与会话",
  summary: (n: number) => `${n} 个会话在线`,
  othersOut: "改口令后，其他设备会被登出",
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
  revoke: "让它退出",
  revokeAll: "让所有设备退出",
};

const en: typeof zh = {
  ...common.en,
  title: "Sign-in & sessions",
  summary: (n) => `${n} sessions signed in`,
  othersOut: "Other devices are signed out after a password change",
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
  revoke: "Sign it out",
  revokeAll: "Sign out all devices",
};

export const strings: Dict<typeof zh> = { zh, en };
