import type { Feature } from "../../app/registry";
import { SettingsPolicyPage } from "./SettingsPolicyPage";

export const feature: Feature = { id: "settings-policy", title: "策略与人格", path: "settings", order: 2, section: "settings", element: <SettingsPolicyPage /> };
