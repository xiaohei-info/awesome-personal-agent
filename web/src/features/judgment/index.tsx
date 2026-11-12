import type { Feature } from "../../app/registry";
import { JudgmentPage } from "./JudgmentPage";

export const feature: Feature = { id: "judgment", title: "判断与校准", path: "judgment", order: 30, group: "规则", element: <JudgmentPage /> };
