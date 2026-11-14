import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "Web 未读",
  lede: "退回 Web、你还没看过的通知。",
  empty: "没有未读",
  options: (o: string) => `选项：${o}`,
  open: "打开",
  injected: (n: number) => `已注入 ${n} 个会话（注入不算确认，仍需你本人确认）`,
  markRead: "标记已读",
};

const en: typeof zh = {
  ...common.en,
  title: "Web unread",
  lede: "Notifications that fell back to the Web and you have not seen yet.",
  empty: "Nothing unread",
  options: (o) => `Options: ${o}`,
  open: "Open",
  injected: (n) => `Injected into ${n} session${n === 1 ? "" : "s"} (an injection is not a confirmation; you still need to confirm)`,
  markRead: "Mark as read",
};

export const strings: Dict<typeof zh> = { zh, en };
