import type { Feature } from "../../app/registry";
import type { Doc } from "../settings-policy/shared";
import { SettingsNotifyPage } from "./SettingsNotifyPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-notify",
  title: { zh: "提醒与打扰", en: "Notifications" },
  path: "settings/notify",
  order: 1,
  section: "settings",
  summary: async (api, lang) => {
    const d = (await api.get<Doc>("/settings/policy")).disturb ?? {};
    const cap = d.daily_cap ?? 20, muted = (d.mutes ?? []).length;
    return strings[lang].summary(cap, muted);
  },
  element: <SettingsNotifyPage />,
};
