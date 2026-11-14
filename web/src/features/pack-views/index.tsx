import type { Feature } from "../../app/registry";
import { PackViewsPage } from "./PackViewsPage";

export const feature: Feature = { id: "pack-views", title: "能力包视图", path: "pack-views", order: 1, section: "items", element: <PackViewsPage /> };
