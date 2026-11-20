import type { Feature } from "../../app/registry";
import { DelegationPage } from "./DelegationPage";

// Agent 工作 › 进行中（界面设计 §3.2）：我交出去的事现在怎样——后台运行中的事与派给执行者的活。原地址不变。
export const feature: Feature = { id: "delegation", title: { zh: "进行中", en: "In progress" }, path: "delegation", order: 0, section: "agents", element: <DelegationPage /> };
