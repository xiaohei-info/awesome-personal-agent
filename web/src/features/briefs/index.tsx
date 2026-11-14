import type { Feature } from "../../app/registry";
import { BriefsPage } from "./BriefsPage";

export const feature: Feature = { id: "briefs", title: { zh: "简报", en: "Briefs" }, path: "briefs", order: 1, section: "today", element: <BriefsPage /> };
