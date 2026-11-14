import type { Feature } from "../../app/registry";
import { vocab } from "../../app/vocab";
import type { Doc } from "../settings-policy/shared";
import { SettingsAccessPage } from "./SettingsAccessPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-access",
  title: { zh: "后台权限", en: "Background access" },
  path: "settings/access",
  order: 3,
  section: "settings",
  summary: async (api, lang) => {
    const [lim, pol] = await Promise.all([api.get<Doc>("/settings/access-limits"), api.get<Doc>("/settings/policy")]);
    const level = lim.permission_cap?.default ?? "read";
    return strings[lang].summary((vocab[lang].perm as Record<string, { label: string }>)[level]?.label ?? level, (pol.default_harness as string) ?? "");
  },
  element: <SettingsAccessPage />,
};
