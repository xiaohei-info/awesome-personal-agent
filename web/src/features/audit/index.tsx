import type { Feature } from "../../app/registry";
import { AuditPage } from "./AuditPage";

export const feature: Feature = { id: "audit", title: { zh: "操作记录", en: "Activity log" }, path: "audit", order: 1, section: "diag", element: <AuditPage /> };
