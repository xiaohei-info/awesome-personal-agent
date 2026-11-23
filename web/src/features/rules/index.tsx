import type { Feature } from "../../app/registry";
import { RulesPage } from "./RulesPage";

// 设置 › 日常 ›「自动动手」（原侧边栏 › 自动规则 › 规则）：规则本人不能新建或编辑，只切模式、选 Agent，属于设置（§0.1）。
// 提醒类规则（跟进、停滞）不在这里，它们的开关在「提醒与推送」矩阵的对应行；旧地址 /rules 改写到这里。
export const feature: Feature = {
  id: "rules", title: { zh: "自动动手", en: "Act for me" }, path: "settings/rules", aliases: ["rules"], order: 2.5, section: "settings", element: <RulesPage />,
};
