import type { Feature } from "../../app/registry";
import type { Doc } from "../settings-policy/shared";
import { SettingsMergePage } from "./SettingsMergePage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-merge",
  title: { zh: "跨来源合并", en: "Cross-source merge" },
  path: "settings/merge",
  order: 8,
  section: "settings",
  summary: async (api, lang) => {
    const d = await api.get<Doc>("/settings/merge");
    return strings[lang].summary(d.merge_judge !== "off", !!d.merge?.prompt);
  },
  element: <SettingsMergePage />,
};
