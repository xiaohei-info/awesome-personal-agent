import type { Feature } from "../../app/registry";
import { AuditPage } from "./AuditPage";

export const feature: Feature = { id: "audit", title: "审计", path: "audit", order: 90, element: <AuditPage /> };
