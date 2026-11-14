import type { Feature } from "../../app/registry";
import { AuditPage } from "./AuditPage";

export const feature: Feature = { id: "audit", title: { zh: "审计", en: "Audit" }, path: "audit", order: 1, section: "health", element: <AuditPage /> };
