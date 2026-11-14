import type { Feature } from "../../app/registry";
import { SettingsModelsPage } from "./SettingsModelsPage";

export const feature: Feature = { id: "settings-models", title: "模型", path: "settings/models", order: 1, section: "settings", element: <SettingsModelsPage /> };
