import type { Feature } from "../../app/registry";
import { TodayPage } from "./TodayPage";

// 原「未读提醒」并进今天页的提醒区（界面设计 §3.1），旧地址 /unread 改写过来。
export const feature: Feature = { id: "today", title: { zh: "今天", en: "Today" }, path: "today", order: 0, section: "today", aliases: ["unread"], element: <TodayPage /> };
