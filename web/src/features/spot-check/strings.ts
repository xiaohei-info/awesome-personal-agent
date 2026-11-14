import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "抽查漏掉的",
  lede: "被自动跳过的消息里，有没有该看的？",
  list: "待抽查",
  done: "都看过了",
  shouldSee: "该看（转成建议）",
  correct: "确实不用",
  reviewed: (subject: string, result: string) => `「${subject}」：${result}`,
  noSubject: "（无标题）",
  perBrief: "每份简报抽",
  perBriefUnit: "条",
  perBriefSaved: (n: number) => `已改为每份简报抽 ${n} 条`,
  perBriefInvalid: "请填不小于 0 的整数",
};

const en: typeof zh = {
  ...common.en,
  title: "Missed checks",
  lede: "Did anything skipped automatically deserve a look?",
  list: "To check",
  done: "All checked",
  shouldSee: "Should see (make a proposal)",
  correct: "Fine to skip",
  reviewed: (subject, result) => `"${subject}": ${result}`,
  noSubject: "(no subject)",
  perBrief: "Check per brief",
  perBriefUnit: "items",
  perBriefSaved: (n) => `Now ${n} per brief`,
  perBriefInvalid: "Enter a whole number, 0 or more",
};

export const strings: Dict<typeof zh> = { zh, en };
