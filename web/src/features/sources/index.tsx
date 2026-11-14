import type { Feature } from "../../app/registry";
import { SourcesPage } from "./SourcesPage";

export const feature: Feature = { id: "sources", title: { zh: "来源", en: "Sources" }, path: "sources", order: 0, section: "sources", element: <SourcesPage /> };
