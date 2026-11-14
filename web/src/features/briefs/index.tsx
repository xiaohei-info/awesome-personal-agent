import type { Feature } from "../../app/registry";
import { BriefsPage } from "./BriefsPage";

export const feature: Feature = { id: "briefs", title: "简报", path: "briefs", order: 1, section: "today", element: <BriefsPage /> };
