import type { Feature } from "../../app/registry";
import { PurgePage } from "./PurgePage";

export const feature: Feature = { id: "purge", title: "物理删除", path: "purge", order: 42, group: "系统", element: <PurgePage /> };
