import type { Feature } from "../../app/registry";
import { SettingsChannelsPage, channelNames } from "./SettingsChannelsPage";
import { strings } from "./strings";

export const feature: Feature = {
  id: "settings-channels",
  title: { zh: "渠道", en: "Channels" },
  path: "settings/channels",
  order: 7,
  section: "settings",
  summary: async (api, lang) => strings[lang].summary((await channelNames(api)).length),
  element: <SettingsChannelsPage />,
};
