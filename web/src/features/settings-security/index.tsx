import type { Feature } from "../../app/registry";
import { SettingsSecurityPage } from "./SettingsSecurityPage";

export const feature: Feature = { id: "settings-security", title: "登录安全", path: "settings-security", order: 29, group: "系统", element: <SettingsSecurityPage /> };
