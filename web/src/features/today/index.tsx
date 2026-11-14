import type { Feature } from "../../app/registry";
import { TodayPage } from "./TodayPage";

export const feature: Feature = { id: "today", title: { zh: "今天", en: "Today" }, path: "today", order: 0, section: "today", element: <TodayPage /> };
