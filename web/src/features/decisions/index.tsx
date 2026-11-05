import type { Feature } from "../../app/registry";
import { DecisionsPage } from "./DecisionsPage";

export const feature: Feature = { id: "decisions", title: "决策待办", path: "decisions", order: 1, element: <DecisionsPage /> };
