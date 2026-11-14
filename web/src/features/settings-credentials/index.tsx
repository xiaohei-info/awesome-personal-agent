import type { Feature } from "../../app/registry";
import { SettingsCredentialsPage } from "./SettingsCredentialsPage";

export const feature: Feature = { id: "settings-credentials", title: "凭据", path: "settings-credentials", order: 3, section: "settings", element: <SettingsCredentialsPage /> };
