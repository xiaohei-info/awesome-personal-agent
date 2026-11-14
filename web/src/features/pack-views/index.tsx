import type { Feature } from "../../app/registry";
import { PackViewsPage } from "./PackViewsPage";

export const feature: Feature = { id: "pack-views", title: { zh: "能力包视图", en: "Pack views" }, path: "pack-views", order: 1, section: "items", element: <PackViewsPage /> };
