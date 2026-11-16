import type { Feature } from "../../app/registry";
import { SettingsStoragePage, fmtBytes } from "./SettingsStoragePage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-storage",
  title: { zh: "存储", en: "Storage" },
  path: "settings/storage",
  order: 8,
  section: "settings",
  summary: async (api, lang) => {
    const s = await api.get<{ ledger_bytes: number; blobs_bytes: number; content_bytes: number }>("/storage");
    return strings[lang].summary(fmtBytes(s.ledger_bytes + s.blobs_bytes + s.content_bytes));
  },
  element: <SettingsStoragePage />,
};
