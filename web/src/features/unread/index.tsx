import type { Feature } from "../../app/registry";
import { UnreadPage } from "./UnreadPage";

export const feature: Feature = { id: "unread", title: "Web 未读", path: "unread", order: 2, section: "today", element: <UnreadPage /> };
