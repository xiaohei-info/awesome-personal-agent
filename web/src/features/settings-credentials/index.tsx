import type { Feature } from "../../app/registry";
import { SettingsCredentialsPage } from "./SettingsCredentialsPage";

export const feature: Feature = { id: "settings-credentials", title: { zh: "凭据", en: "Credentials" }, path: "settings-credentials", order: 3, section: "settings", element: <SettingsCredentialsPage /> };
