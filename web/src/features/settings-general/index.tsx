import type { Feature } from "../../app/registry";
import { LANG_NAMES } from "../../app/strings";
import type { Doc } from "../settings-policy/shared";
import { SettingsGeneralPage } from "./SettingsGeneralPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-general",
  title: { zh: "通用", en: "General" },
  path: "settings/general",
  order: 0,
  section: "settings",
  summary: async (api, lang) => strings[lang].summary(LANG_NAMES[lang], !!(await api.get<Doc>("/settings/policy")).persona?.trim()),
  element: <SettingsGeneralPage />,
};
