import type { Feature } from "../../app/registry";
import { HealthPage } from "./HealthPage";

export const feature: Feature = { id: "health", title: { zh: "状态", en: "Status" }, path: "health", order: 0, section: "diag", element: <HealthPage /> };
