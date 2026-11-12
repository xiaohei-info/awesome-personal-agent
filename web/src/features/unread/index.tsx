import type { Feature } from "../../app/registry";
import { UnreadPage } from "./UnreadPage";

export const feature: Feature = { id: "unread", title: "Web 未读", path: "unread", order: 3, group: "今天", element: <UnreadPage /> };
