import type { Feature } from "../../app/registry";
import { SettingsOutwardApiPage } from "./SettingsOutwardApiPage";
import { strings } from "./strings";

type Creds = { credentials: { revoked_at?: string; scope?: string }[] };
export const feature: Feature = {
  id: "settings-outward-api",
  title: { zh: "对外接口", en: "Outward API" },
  path: "settings/outward-api",
  order: 8.2,
  section: "settings",
  summary: async (api, lang) => {
    const r = await api.get<Creds>("/settings/ext-keys");
    return strings[lang].summary(r.credentials.filter((c) => !c.revoked_at && (c.scope === "forward" || c.scope?.split(".")[0] === "export")).length);
  },
  element: <SettingsOutwardApiPage />,
};
