import type { Feature } from "../../app/registry";
import { ItemsPage } from "./ItemsPage";

export const feature: Feature = { id: "items", title: { zh: "事项", en: "Items" }, path: "items", order: 0, section: "items", element: <ItemsPage /> };
