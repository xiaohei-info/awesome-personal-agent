import type { Feature } from "../../app/registry";
import { JudgmentPage } from "./JudgmentPage";

export const feature: Feature = { id: "judgment", title: { zh: "筛选门槛", en: "Thresholds" }, path: "judgment", order: 1, section: "automation", element: <JudgmentPage /> };
