import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  submitted: "已提交",
  submittedBody: "你的回答已记录，此链接已失效，可以关闭本页。",
  expired: "链接已失效",
  expiredBody: (why: string) => `此链接已被使用、已过期或对应事项已变化（${why}）。请使用最新消息中的链接，或在 Web 中作答。`,
  useWeb: "请在 Web 中作答",
  goWeb: "前往 Web 登录并作答",
  loadFailed: (why: string) => `无法加载表单：${why}`,
  options: "选项",
  note: "补充说明",
  reason: "理由（可选）",
  submit: "提交",
  once: "此链接只能提交一次。",
};

const en: typeof zh = {
  ...common.en,
  submitted: "Submitted",
  submittedBody: "Your answer is recorded and this link no longer works. You can close this page.",
  expired: "Link no longer valid",
  expiredBody: (why) => `This link was already used, has expired, or the item has changed (${why}). Use the link in the latest message, or answer in the Web UI.`,
  useWeb: "Please answer in the Web UI",
  goWeb: "Sign in to the Web UI and answer",
  loadFailed: (why) => `Could not load the form: ${why}`,
  options: "Options",
  note: "Additional note",
  reason: "Reason (optional)",
  submit: "Submit",
  once: "This link can be submitted only once.",
};

export const strings: Dict<typeof zh> = { zh, en };
