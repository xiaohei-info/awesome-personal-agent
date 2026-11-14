import type { Feature } from "../../app/registry";
import { SettingsPolicyPage } from "./SettingsPolicyPage";

export const feature: Feature = { id: "settings-policy", title: { zh: "策略与人格", en: "Policy & persona" }, path: "settings", order: 2, section: "settings", element: <SettingsPolicyPage /> };
