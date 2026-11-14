import type { Feature } from "../../app/registry";
import { SettingsModelsPage } from "./SettingsModelsPage";

export const feature: Feature = { id: "settings-models", title: { zh: "模型", en: "Models" }, path: "settings/models", order: 1, section: "settings", element: <SettingsModelsPage /> };
