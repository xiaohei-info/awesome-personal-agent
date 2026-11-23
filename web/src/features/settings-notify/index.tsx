import type { Feature } from "../../app/registry";
import type { Doc } from "../settings-policy/shared";
import { SettingsNotifyPage } from "./SettingsNotifyPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-notify",
  title: { zh: "提醒与推送", en: "Reminders and pushes" },
  path: "settings/notify",
  order: 1,
  section: "settings",
  summary: async (api, lang) => {
    const d = (await api.get<Doc>("/settings/policy")).disturb ?? {};
    const mutes = (d.mutes ?? []) as { scope: string; until?: string }[];
    const all = mutes.some((m) => m.scope === "all" && (!m.until || new Date(m.until) > new Date()));
    return strings[lang].summary(d.daily_cap ?? 20, mutes.filter((m) => m.scope !== "all").length, all);
  },
  element: <SettingsNotifyPage />,
};
