import type { Feature } from "../../app/registry";
import { RulesPage } from "./RulesPage";

export const feature: Feature = { id: "rules", title: { zh: "规则模式", en: "Rule modes" }, path: "rules", order: 0, section: "automation", element: <RulesPage /> };
