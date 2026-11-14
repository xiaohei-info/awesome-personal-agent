import type { Feature } from "../../app/registry";
import { ProposalsPage, countProposals } from "./ProposalsPage";

export const feature: Feature = { id: "proposals", title: "建议流", path: "proposals", order: 0, section: "proposals", count: countProposals, element: <ProposalsPage /> };
