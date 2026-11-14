import type { Feature } from "../../app/registry";
import { MaterialsPage } from "./MaterialsPage";

export const feature: Feature = { id: "materials", title: { zh: "材料", en: "Materials" }, path: "materials", order: 1, section: "projects", element: <MaterialsPage /> };
