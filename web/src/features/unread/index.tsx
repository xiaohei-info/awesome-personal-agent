import type { Feature } from "../../app/registry";
import { UnreadPage } from "./UnreadPage";

export const feature: Feature = { id: "unread", title: { zh: "Web 未读", en: "Web unread" }, path: "unread", order: 2, section: "today", element: <UnreadPage /> };
