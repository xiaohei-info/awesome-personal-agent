import type { Feature } from "../../app/registry";
import { PurgePage } from "./PurgePage";

export const feature: Feature = { id: "purge", title: { zh: "永久删除", en: "Permanent deletion" }, path: "purge", order: 9, section: "settings", element: <PurgePage /> };
