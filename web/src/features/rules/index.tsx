import type { Feature } from "../../app/registry";
import { RulesPage } from "./RulesPage";

export const feature: Feature = { id: "rules", title: "规则模式", path: "rules", order: 0, section: "automation", element: <RulesPage /> };
