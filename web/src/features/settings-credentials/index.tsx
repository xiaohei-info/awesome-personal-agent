import type { Feature } from "../../app/registry";
import { SettingsCredentialsPage } from "./SettingsCredentialsPage";

export const feature: Feature = { id: "settings-credentials", title: "凭据", path: "settings-credentials", order: 21, group: "接入", element: <SettingsCredentialsPage /> };
