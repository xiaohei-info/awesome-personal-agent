import type { Feature } from "../../app/registry";
import { DecisionsPage, countDecisions } from "./DecisionsPage";

export const feature: Feature = { id: "decisions", title: "决策待办", path: "decisions", order: 1, group: "今天", count: countDecisions, element: <DecisionsPage /> };
