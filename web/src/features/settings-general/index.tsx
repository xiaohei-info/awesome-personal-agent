import type { Feature } from "../../app/registry";
import { SettingsGeneralPage } from "./SettingsGeneralPage";

export const feature: Feature = { id: "settings-general", title: { zh: "通用", en: "General" }, path: "settings/general", order: 0, section: "settings", element: <SettingsGeneralPage /> };
