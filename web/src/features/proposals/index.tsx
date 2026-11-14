import type { Feature } from "../../app/registry";
import { ProposalsPage, countProposals } from "./ProposalsPage";

export const feature: Feature = { id: "proposals", title: { zh: "建议流", en: "Proposals" }, path: "proposals", order: 0, section: "proposals", count: countProposals, element: <ProposalsPage /> };
