import type { Feature } from "../../app/registry";
import { RunsPage } from "./RunsPage";

export const feature: Feature = { id: "runs", title: { zh: "后台运行", en: "Background runs" }, path: "runs", order: 2, section: "diag", element: <RunsPage /> };
