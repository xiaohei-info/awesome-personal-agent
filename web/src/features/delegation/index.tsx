import type { Feature } from "../../app/registry";
import { DelegationPage } from "./DelegationPage";

export const feature: Feature = { id: "delegation", title: "任务委派", path: "delegation", order: 13, group: "工作", element: <DelegationPage /> };
