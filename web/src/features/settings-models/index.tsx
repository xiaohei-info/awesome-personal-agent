import type { Feature } from "../../app/registry";
import { SettingsModelsPage } from "./SettingsModelsPage";
import { strings } from "./strings";

type Doc = { judge_endpoint: unknown; generate_endpoint: unknown; models_paused?: boolean };
export const feature: Feature = {
  id: "settings-models",
  title: { zh: "模型", en: "Models" },
  path: "settings/models",
  order: 7,
  section: "settings",
  summary: async (api, lang) => {
    const d = await api.get<Doc>("/settings/model-endpoints");
    const t = strings[lang];
    return !d.judge_endpoint && !d.generate_endpoint ? t.noneYet : d.models_paused ? t.paused : t.running;
  },
  element: <SettingsModelsPage />,
};
