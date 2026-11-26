import type { Feature } from "../../app/registry";
import { DEFAULT_BATCH, DEFAULT_WAIT, SettingsSourcePipelinePage, type SourcePipeline } from "./SettingsSourcePipelinePage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-source-pipeline",
  title: { zh: "来源处理", en: "Source handling" },
  path: "settings/source-pipeline",
  order: 8,
  section: "settings",
  summary: async (api, lang) => {
    const d = await api.get<{ source_pipeline?: SourcePipeline }>("/settings/source-pipeline");
    const sp = d.source_pipeline ?? {};
    return strings[lang].summary(sp.batch_size || DEFAULT_BATCH, sp.wait_minutes || DEFAULT_WAIT, !!sp.urgent_immediate);
  },
  element: <SettingsSourcePipelinePage />,
};
