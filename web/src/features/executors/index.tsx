import type { Feature } from "../../app/registry";
import { ExecutorsPage } from "./ExecutorsPage";

// 执行者名录（原「Agent 工作 › 执行者」，界面设计 §9.6）：不进导航，地址不变；从设置 › Agent 列表的「很少用到」进入。
export const feature: Feature = { id: "executors", title: { zh: "执行者", en: "Executors" }, path: "executors", hidden: true, element: <ExecutorsPage /> };
