import type { Feature } from "../../app/registry";
import { AgentChangesPage } from "./AgentChangesPage";

export const feature: Feature = { id: "agent-changes", title: { zh: "Agent 改动", en: "Agent changes" }, path: "agent-changes", order: 1.2, section: "diag", element: <AgentChangesPage /> };
