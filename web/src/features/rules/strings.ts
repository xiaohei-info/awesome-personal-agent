import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "规则模式",
  lede: "observe 只记录“本来会做什么”；审阅后由本人切到 auto。",
  noObservations: "暂无观察记录",
  observations: (k: string) => `观察记录 ${k}`,
  wouldDo: (v: string) => ` · 本来会做：${v}`,
  none: "没有已加载的规则",
  mode: (k: string) => `模式 ${k}`,
  review: (k: string) => `审阅观察记录 ${k}`,
  showObs: "审阅观察记录",
  hideObs: "收起观察记录",
};

const en: typeof zh = {
  ...common.en,
  title: "Rule modes",
  lede: "observe only records what a rule would have done; switch it to auto after you review.",
  noObservations: "No observations yet",
  observations: (k) => `Observations ${k}`,
  wouldDo: (v) => ` · would have done: ${v}`,
  none: "No rules loaded",
  mode: (k) => `Mode ${k}`,
  review: (k) => `Review observations ${k}`,
  showObs: "Review observations",
  hideObs: "Hide observations",
};

export const strings: Dict<typeof zh> = { zh, en };
