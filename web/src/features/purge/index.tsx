import type { Feature } from "../../app/registry";
import { PurgePage } from "./PurgePage";

export const feature: Feature = { id: "purge", title: { zh: "物理删除", en: "Permanent deletion" }, path: "purge", order: 7, section: "settings", element: <PurgePage /> };
