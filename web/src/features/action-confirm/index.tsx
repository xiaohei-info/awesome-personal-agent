import type { Feature } from "../../app/registry";
import { ActionConfirm } from "./ActionConfirm";

// 动作链接 /act/<事项 id>?do=done|progress|plan 的确认页（附录 B.2）；只作为链接目标，不进导航。
export const feature: Feature = { id: "action-confirm", title: "动作确认", path: "act", hidden: true, element: <ActionConfirm /> };
