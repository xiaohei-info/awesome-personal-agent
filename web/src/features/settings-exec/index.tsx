import type { Feature } from "../../app/registry";
import { SettingsExecPage } from "./SettingsExecPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-exec",
  title: { zh: "宿主接入", en: "Host access" },
  path: "settings-exec",
  order: 6,
  section: "settings",
  summary: async (api, lang) => {
    const pend = await api.get<{ declarations: unknown[] }>("/settings/declarations");
    return strings[lang].summary(pend.declarations.length);
  },
  element: <SettingsExecPage />,
};
