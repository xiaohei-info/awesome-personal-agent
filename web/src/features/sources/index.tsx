import type { Feature } from "../../app/registry";
import { SourcesPage } from "./SourcesPage";

export const feature: Feature = { id: "sources", title: "来源", path: "sources", order: 0, section: "sources", element: <SourcesPage /> };
