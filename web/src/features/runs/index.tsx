import type { Feature } from "../../app/registry";
import { RunsPage } from "./RunsPage";

export const feature: Feature = { id: "runs", title: "运行记录", path: "runs", order: 2, section: "agents", element: <RunsPage /> };
