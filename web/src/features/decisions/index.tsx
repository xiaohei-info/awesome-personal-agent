import type { Feature } from "../../app/registry";
import { DecisionsPage } from "./DecisionsPage";

// 入口与阶段都叫「等你决定」（界面设计 §8.1）；红色数字 = 问题数 + 待确认条数，取自注意力汇总（§5.2）。
export const feature: Feature = { id: "decisions", title: { zh: "等你决定", en: "For you to decide" }, path: "decisions", order: 0, section: "decisions", badge: "decide", element: <DecisionsPage /> };
