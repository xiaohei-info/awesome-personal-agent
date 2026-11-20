import type { Feature } from "../../app/registry";
import { CalendarPage } from "./CalendarPage";

// 日历挂在"今天"入口的页签里（今天、日历、简报，界面设计 §3.1）：它是事项的另一种读法，只读。
export const feature: Feature = { id: "calendar", title: { zh: "日历", en: "Calendar" }, path: "calendar", order: 0.5, section: "today", element: <CalendarPage /> };
