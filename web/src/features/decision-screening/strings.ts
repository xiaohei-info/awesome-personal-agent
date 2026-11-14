import type { Dict } from "../../app/i18n";

const zh = {
  screening: "初筛中",
  screeningTip: "初筛返回前不投递、不接受代答，只有你能在这里直接回答",
};
const en: typeof zh = {
  screening: "Screening",
  screeningTip: "Until screening returns, nothing is delivered and no one can answer on your behalf; only you can answer here",
};

export const strings: Dict<typeof zh> = { zh, en };
