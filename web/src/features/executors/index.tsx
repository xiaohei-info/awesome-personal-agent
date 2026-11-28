import type { Feature } from "../../app/registry";
import { ExecutorsPage } from "./ExecutorsPage";

// 执行者名录（界面设计 §9.6）：侧栏「自动化 › 执行者」的默认页签，旁边是「委派」页签；地址不变。
export const feature: Feature = { id: "executors", title: { zh: "执行者", en: "Executors" }, path: "executors", order: 0, section: "executors", element: <ExecutorsPage /> };
