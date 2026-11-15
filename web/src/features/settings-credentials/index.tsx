import type { Feature } from "../../app/registry";
import { SettingsCredentialsPage } from "./SettingsCredentialsPage";
import { strings } from "./strings";

type Creds = { credentials: { revoked_at?: string; scope?: string }[] };
export const feature: Feature = {
  id: "settings-credentials",
  title: { zh: "扩展程序", en: "Extensions" },
  path: "settings-credentials",
  order: 8,
  section: "settings",
  summary: async (api, lang) => {
    const r = await api.get<Creds>("/settings/ext-keys");
    return strings[lang].summary(r.credentials.filter((c) => !c.revoked_at && !c.scope?.startsWith("channel.")).length);
  },
  element: <SettingsCredentialsPage />,
};
