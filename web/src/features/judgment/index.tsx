import type { Feature } from "../../app/registry";
import { JudgmentPage } from "./JudgmentPage";

export const feature: Feature = { id: "judgment", title: { zh: "判断与校准", en: "Judgment & calibration" }, path: "judgment", order: 1, section: "automation", aliases: ["judgments"], element: <JudgmentPage /> };
