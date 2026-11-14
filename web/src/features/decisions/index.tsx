import type { Feature } from "../../app/registry";
import { DecisionsPage, countDecisions } from "./DecisionsPage";

export const feature: Feature = { id: "decisions", title: "决策待办", path: "decisions", order: 0, section: "decisions", count: countDecisions, element: <DecisionsPage /> };
