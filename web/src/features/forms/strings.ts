import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  submitted: "已提交",
  submittedBody: "回答已记录，可以关闭本页。",
  expired: "链接已失效",
  expiredBody: "请用最新消息里的链接，或到网页上作答。",
  useWeb: "请在网页上作答",
  goWeb: "登录网页去作答",
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
  submittedBody: "Your answer is recorded. You can close this page.",
  expired: "Link no longer valid",
  expiredBody: "Use the link in the latest message, or answer on the web.",
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
