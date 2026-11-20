import type { Feature } from "../../app/registry";
import { ProposalsPage } from "./ProposalsPage";

// 建议的数字是灰色：不处理也不会卡住任何事（界面设计 §5.1）。
export const feature: Feature = { id: "proposals", title: { zh: "建议", en: "Proposals" }, path: "proposals", order: 0, section: "proposals", badge: "proposals", element: <ProposalsPage /> };
