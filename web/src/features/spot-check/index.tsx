import type { Feature } from "../../app/registry";
import type { ApiClient } from "../../app/api";
import { SpotCheckPage } from "./SpotCheckPage";

// 原「判断与校准」里的抽检，作为消息来源的页签（设计 §3.2）；简报抽检条目的 /judgments/spot-check?judgment=… 改写到这里。
const countPool = (api: ApiClient) => api.get<{ spot_check: unknown[] }>("/judgments/spot-check").then((r) => r.spot_check.length);

export const feature: Feature = { id: "spot-check", title: { zh: "抽查漏掉的", en: "Missed checks" }, path: "spot-check", order: 1, section: "sources", aliases: ["judgments"], count: countPool, element: <SpotCheckPage /> };
