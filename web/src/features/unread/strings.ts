import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "未读提醒",
  empty: "都看过了",
  options: (o: string) => `选项：${o}`,
  open: "打开",
  injected: (n: number) => `已在 ${n} 次聊天里提过，仍未确认`,
  markRead: "标记已读",
};

const en: typeof zh = {
  ...common.en,
  title: "Unread reminders",
  empty: "All caught up",
  options: (o) => `Options: ${o}`,
  open: "Open",
  injected: (n) => `Mentioned in ${n} chat${n === 1 ? "" : "s"}; not confirmed yet`,
  markRead: "Mark as read",
};

export const strings: Dict<typeof zh> = { zh, en };
