import type { Feature } from "../../app/registry";
import { SettingsExecPage } from "./SettingsExecPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-exec",
  title: { zh: "执行者", en: "Executors" },
  path: "settings-exec",
  order: 6,
  section: "settings",
  summary: async (api, lang) => {
    const [ex, pend] = await Promise.all([api.get<{ executors: unknown[] }>("/settings/executors"), api.get<{ declarations: unknown[] }>("/settings/declarations")]);
    return strings[lang].summary(ex.executors.length, pend.declarations.length);
  },
  element: <SettingsExecPage />,
};
