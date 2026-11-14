import type { Feature } from "../../app/registry";
import { SettingsGrantsPage } from "./SettingsGrantsPage";

export const feature: Feature = { id: "settings-grants", title: { zh: "授权单", en: "Grants" }, path: "settings/grants", order: 5, section: "settings", element: <SettingsGrantsPage /> };
