import type { Feature } from "../../app/registry";
import { JudgmentPage } from "./JudgmentPage";

// 筛选门槛管的是消息怎么筛，归在消息来源下（界面设计 §3.1）。
export const feature: Feature = { id: "judgment", title: { zh: "筛选门槛", en: "Thresholds" }, path: "judgment", order: 1, section: "sources", element: <JudgmentPage /> };
