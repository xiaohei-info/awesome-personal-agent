import type { Feature } from "../../app/registry";
import { RunsPage } from "./RunsPage";

export const feature: Feature = { id: "runs", title: { zh: "运行记录", en: "Runs" }, path: "runs", order: 2, section: "agents", element: <RunsPage /> };
