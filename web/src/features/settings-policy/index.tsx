import type { Feature } from "../../app/registry";
import type { AgentView } from "../settings-agents/AgentPick";
import { SettingsPolicyPage } from "./SettingsPolicyPage";
import { strings } from "./strings";

// 路径沿用原「策略与人格」的 /settings，旧书签与链接直接落到这里。
export const feature: Feature = {
  id: "settings-policy",
  title: { zh: "权限总览", en: "Permissions" },
  path: "settings",
  order: 2,
  section: "settings",
  summary: async (api, lang) => {
    const v = await api.get<AgentView>("/agents");
    const n = v.agents.filter((a) => a.permission?.groups.decide).length;
    return strings[lang].summary(n);
  },
  element: <SettingsPolicyPage />,
};
