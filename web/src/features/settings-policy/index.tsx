import type { Feature } from "../../app/registry";
import type { Doc } from "./shared";
import { SettingsPolicyPage } from "./SettingsPolicyPage";
import { strings } from "./strings";

// 路径沿用原「策略与人格」的 /settings，旧书签与链接直接落到这里。
export const feature: Feature = {
  id: "settings-policy",
  title: { zh: "我来拍板", en: "Your call" },
  path: "settings",
  order: 2,
  section: "settings",
  summary: async (api, lang) => {
    const p = await api.get<Doc>("/settings/policy");
    const n = (p.web_only?.categories ?? []).length;
    return strings[lang].summary(n);
  },
  element: <SettingsPolicyPage />,
};
