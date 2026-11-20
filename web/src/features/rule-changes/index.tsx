import type { Feature } from "../../app/registry";
import { RuleChangesPage } from "./RuleChangesPage";

// 规矩改动（设计 §14.3 的 3a 兜底）：放在「Agent 工作」里，紧挨「改动」。
export const feature: Feature = { id: "rule-changes", title: { zh: "规矩改动", en: "Rule changes" }, path: "rule-changes", order: 1.5, section: "agents", element: <RuleChangesPage /> };
