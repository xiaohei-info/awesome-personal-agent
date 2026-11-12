import type { Feature } from "../../app/registry";
import { ProposalsPage } from "./ProposalsPage";

export const feature: Feature = { id: "proposals", title: "建议流", path: "proposals", order: 2, group: "今天", element: <ProposalsPage /> };
