import type { Feature } from "../../app/registry";
import { SettingsCredentialsPage } from "./SettingsCredentialsPage";
import { strings } from "./strings";

type Creds = { credentials: { revoked_at?: string }[] };
export const feature: Feature = {
  id: "settings-credentials",
  title: { zh: "接入钥匙", en: "Access keys" },
  path: "settings-credentials",
  order: 5,
  section: "settings",
  summary: async (api, lang) => {
    const [a, b] = await Promise.all([api.get<Creds>("/settings/tokens"), api.get<Creds>("/settings/ext-keys")]);
    return strings[lang].summary([...a.credentials, ...b.credentials].filter((c) => !c.revoked_at).length);
  },
  element: <SettingsCredentialsPage />,
};
