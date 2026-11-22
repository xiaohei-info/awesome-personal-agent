import type { Feature } from "../../app/registry";
import { RuleChangesPage } from "./RuleChangesPage";

// 规矩改动（设计 §14.3 的 3a 兜底）：放在诊断里，紧挨「Agent 改动」；撤回是这份记录自己的动作。
export const feature: Feature = { id: "rule-changes", title: { zh: "规矩改动", en: "Rule changes" }, path: "rule-changes", order: 1.4, section: "diag", element: <RuleChangesPage /> };
