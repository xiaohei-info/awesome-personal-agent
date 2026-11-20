import type { Feature } from "../../app/registry";
import { SettingsAgentsPage } from "./SettingsAgentsPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-agents",
  title: { zh: "Agent", en: "Agents" },
  path: "settings/agents",
  order: 3,
  section: "settings",
  // 旧的入口（后台权限、已取消的临时授权、宿主接入）改写到这里。
  aliases: ["settings/access", "settings/grants", "settings-exec"],
  summary: async (api, lang) => {
    const [p, c] = await Promise.all([
      api.get<{ declarations: { harness: string }[] }>("/settings/declarations"),
      api.get<{ declarations: { harness: string }[] }>("/settings/commands"),
    ]);
    const n = new Set([...p.declarations, ...c.declarations].map((d) => d.harness)).size;
    return strings[lang].summary(n, p.declarations.length);
  },
  element: <SettingsAgentsPage />,
};
