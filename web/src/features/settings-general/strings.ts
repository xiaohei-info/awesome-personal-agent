import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "通用",
  lede: "这台浏览器上的界面偏好。",
  language: "界面语言",
  auto: "跟随浏览器",
  autoNow: (name: string) => `（现在是 ${name}）`,
  scope: "只改这台浏览器上的界面文字，不跨设备同步。通知、简报和内置提示词仍是中文；服务端返回的原因说明与 CLI 是英文。",
};

const en: typeof zh = {
  ...common.en,
  title: "General",
  lede: "Interface preferences for this browser.",
  language: "Interface language",
  auto: "Follow the browser",
  autoNow: (name) => ` (currently ${name})`,
  scope: "Changes the interface text in this browser only and does not sync across devices. Notifications, briefs and built-in prompts stay in Chinese; reasons returned by the server and the CLI are in English.",
};

export const strings: Dict<typeof zh> = { zh, en };
