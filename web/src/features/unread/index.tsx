import type { Feature } from "../../app/registry";
import { UnreadPage } from "./UnreadPage";

export const feature: Feature = { id: "unread", title: { zh: "未读提醒", en: "Unread reminders" }, path: "unread", order: 2, section: "today", element: <UnreadPage /> };
