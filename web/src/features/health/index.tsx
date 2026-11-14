import type { Feature } from "../../app/registry";
import { HealthPage } from "./HealthPage";

export const feature: Feature = { id: "health", title: { zh: "健康", en: "Health" }, path: "health", order: 0, section: "health", element: <HealthPage /> };
