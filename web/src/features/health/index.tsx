import type { Feature } from "../../app/registry";
import { HealthPage } from "./HealthPage";

export const feature: Feature = { id: "health", title: "健康", path: "health", order: 0, section: "health", element: <HealthPage /> };
