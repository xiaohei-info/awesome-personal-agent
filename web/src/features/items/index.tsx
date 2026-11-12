import type { Feature } from "../../app/registry";
import { ItemsPage } from "./ItemsPage";

export const feature: Feature = { id: "items", title: "事项", path: "items", order: 10, group: "工作", element: <ItemsPage /> };
