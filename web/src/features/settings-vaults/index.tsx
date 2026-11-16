import type { Feature } from "../../app/registry";
import { SettingsVaultsPage } from "./SettingsVaultsPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-vaults",
  title: { zh: "笔记库", en: "Note vaults" },
  path: "settings/note-vaults",
  order: 9,
  section: "settings",
  summary: async (api, lang) => strings[lang].summary((await api.get<{ vaults: unknown[] }>("/settings/note-vaults")).vaults.length),
  element: <SettingsVaultsPage />,
};
