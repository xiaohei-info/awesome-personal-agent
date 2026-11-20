import type { Feature } from "../../app/registry";
import type { ApiClient } from "../../app/api";
import { SpotCheckPage } from "./SpotCheckPage";

// 消息来源的页签（界面设计 §3.1）；简报抽检条目的 /judgments/spot-check?judgment=… 改写到这里。
// 待抽查条数是灰色，只在自己的页签上，不上浮到「消息来源」入口（§5.1）。
const countPool = (api: ApiClient) => api.get<{ spot_check: unknown[] }>("/judgments/spot-check").then((r) => r.spot_check.length);

export const feature: Feature = { id: "spot-check", title: { zh: "抽查", en: "Spot check" }, path: "spot-check", order: 2, section: "sources", aliases: ["judgments"], count: countPool, element: <SpotCheckPage /> };
