import type { Feature } from "../../app/registry";
import { PurgePage } from "./PurgePage";

export const feature: Feature = { id: "purge", title: "物理删除", path: "purge", order: 7, section: "settings", element: <PurgePage /> };
