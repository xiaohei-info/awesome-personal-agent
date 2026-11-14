import type { Feature } from "../../app/registry";
import { SettingsExecPage } from "./SettingsExecPage";

export const feature: Feature = { id: "settings-exec", title: { zh: "执行者与声明", en: "Executors & declarations" }, path: "settings-exec", order: 4, section: "settings", element: <SettingsExecPage /> };
