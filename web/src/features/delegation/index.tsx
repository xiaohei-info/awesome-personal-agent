import type { Feature } from "../../app/registry";
import { DelegationPage } from "./DelegationPage";

// 派给执行者的活（原「Agent 工作 › 进行中」，界面设计 §9.6）：不进导航，地址不变；从状态页「久无回音」与委派事项详情进入。
export const feature: Feature = { id: "delegation", title: { zh: "派给执行者的活", en: "Work handed to executors" }, path: "delegation", hidden: true, element: <DelegationPage /> };
