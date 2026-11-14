import type { Feature } from "../../app/registry";
import { SettingsSecurityPage } from "./SettingsSecurityPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-security",
  title: { zh: "登录与会话", en: "Sign-in & sessions" },
  path: "settings-security",
  order: 8,
  section: "settings",
  summary: async (api, lang) => strings[lang].summary((await api.get<{ sessions: unknown[] }>("/auth/sessions")).sessions.length),
  element: <SettingsSecurityPage />,
};
