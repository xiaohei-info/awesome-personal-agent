import type { Feature } from "../../app/registry";
import { RunsPage } from "./RunsPage";

export const feature: Feature = { id: "runs", title: { zh: "运行日志", en: "Run log" }, path: "runs", order: 2, section: "diag", element: <RunsPage /> };
