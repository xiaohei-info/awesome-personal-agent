import type { Dict } from "../../app/i18n";

const zh = {
  screening: "正在判断风险",
  screeningTip: "判完前只有你能回答",
};
const en: typeof zh = {
  screening: "Checking risk",
  screeningTip: "Until then only you can answer",
};

export const strings: Dict<typeof zh> = { zh, en };
