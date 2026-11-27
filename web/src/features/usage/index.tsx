import type { Feature } from "../../app/registry";
import { UsagePage } from "./UsagePage";

export const feature: Feature = { id: "usage", title: { zh: "用量", en: "Usage" }, path: "usage", section: "usage", element: <UsagePage /> };
