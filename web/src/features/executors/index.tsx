import type { Feature } from "../../app/registry";
import { ExecutorsPage } from "./ExecutorsPage";

export const feature: Feature = { id: "executors", title: { zh: "执行者", en: "Executors" }, path: "executors", order: 2, section: "agents", element: <ExecutorsPage /> };
