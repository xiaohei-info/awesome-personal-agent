import type { Feature } from "../../app/registry";
import { DonePage } from "./DonePage";

// 已完成挂在"今天"入口的页签里（今天、日历、已完成、简报，界面设计 §9.13）：回答"这周我做成了什么"，只读。
export const feature: Feature = { id: "done", title: { zh: "已完成", en: "Done" }, path: "done", order: 0.7, section: "today", element: <DonePage /> };
