import type { Feature } from "../../app/registry";
import { MaterialsPage } from "./MaterialsPage";

export const feature: Feature = { id: "materials", title: "材料", path: "materials", order: 1, section: "projects", element: <MaterialsPage /> };
