import type { Feature } from "../../app/registry";
import { SettingsSecurityPage } from "./SettingsSecurityPage";

export const feature: Feature = { id: "settings-security", title: { zh: "登录安全", en: "Sign-in security" }, path: "settings-security", order: 6, section: "settings", element: <SettingsSecurityPage /> };
