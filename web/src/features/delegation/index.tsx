import type { Feature } from "../../app/registry";
import { DelegationPage } from "./DelegationPage";

// 派给执行者的活（界面设计 §9.6）：「执行者」入口里的「委派」页签；地址不变，状态页「久无回音」与委派事项详情也链到这里。
export const feature: Feature = { id: "delegation", title: { zh: "委派", en: "Delegations" }, path: "delegation", order: 1, section: "executors", element: <DelegationPage /> };
