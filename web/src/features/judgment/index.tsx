import type { Feature } from "../../app/registry";
import { JudgmentPage } from "./JudgmentPage";

export const feature: Feature = { id: "judgment", title: "判断与校准", path: "judgment", order: 1, section: "automation", aliases: ["judgments"], element: <JudgmentPage /> };
