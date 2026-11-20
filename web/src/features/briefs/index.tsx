import type { Feature } from "../../app/registry";
import { BriefsPage, Schedule } from "./BriefsPage";

export const feature: Feature = { id: "briefs", title: { zh: "简报", en: "Briefs" }, path: "briefs", order: 1, section: "today", element: <BriefsPage /> };
// 定时安排（定时简报与例行任务）归在自动规则下（界面设计 §3.1）；地址仍是 /briefs/schedule，旧链接照常可用。
export const schedule: Feature = { id: "schedule", title: { zh: "定时安排", en: "Schedules" }, path: "briefs/schedule", order: 1, section: "automation", element: <Schedule /> };
export const features: Feature[] = [feature, schedule];
