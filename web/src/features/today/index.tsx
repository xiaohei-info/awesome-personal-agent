import type { Feature } from "../../app/registry";
import { TodayPage } from "./TodayPage";

export const feature: Feature = { id: "today", title: "今天", path: "today", order: 0, group: "今天", element: <TodayPage /> };
