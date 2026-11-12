import type { Feature } from "../../app/registry";
import { SettingsGrantsPage } from "./SettingsGrantsPage";

export const feature: Feature = { id: "settings-grants", title: "授权单", path: "settings/grants", order: 23, group: "接入", element: <SettingsGrantsPage /> };
