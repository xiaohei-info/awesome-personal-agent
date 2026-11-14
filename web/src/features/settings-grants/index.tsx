import type { Feature } from "../../app/registry";
import { SettingsGrantsPage } from "./SettingsGrantsPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-grants",
  title: { zh: "临时授权", en: "Temporary grants" },
  path: "settings/grants",
  order: 4,
  section: "settings",
  summary: async (api, lang) => {
    const now = Date.now();
    const r = await api.get<{ grants: { revoked_at?: string; expires_at: string }[] }>("/settings/grants");
    return strings[lang].summary(r.grants.filter((g) => !g.revoked_at && Date.parse(g.expires_at) > now).length);
  },
  element: <SettingsGrantsPage />,
};
