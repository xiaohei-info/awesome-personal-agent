import type { Feature } from "../../app/registry";
import { SettingsSecurityPage } from "./SettingsSecurityPage";

export const feature: Feature = { id: "settings-security", title: "登录安全", path: "settings-security", order: 6, section: "settings", element: <SettingsSecurityPage /> };
