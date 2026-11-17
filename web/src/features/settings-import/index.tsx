import type { Feature } from "../../app/registry";
import { SettingsImportPage } from "./SettingsImportPage";

export const feature: Feature = {
  id: "settings-import",
  title: { zh: "导入", en: "Import" },
  path: "settings/import",
  order: 8.5,
  section: "settings",
  element: <SettingsImportPage />,
};
