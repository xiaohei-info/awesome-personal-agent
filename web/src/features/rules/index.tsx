import type { Feature } from "../../app/registry";
import { RulesPage } from "./RulesPage";

// 设置 › 日常 ›「自动规则」（原「自动动手」）：两组——「提醒我」（到点、截止已过、跟进、14 天没动静，各一个单选，设计 v2 §4）
// 与「替我动手」（动作类规则只切模式、选 Agent）。规则本人不能新建或编辑，属于设置（§0.1）；旧地址 /rules 改写到这里。
export const feature: Feature = {
  id: "rules", title: { zh: "自动规则", en: "Automatic rules" }, path: "settings/rules", aliases: ["rules"], order: 2.5, section: "settings", element: <RulesPage />,
};
