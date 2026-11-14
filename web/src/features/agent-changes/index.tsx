import type { Feature } from "../../app/registry";
import { AgentChangesPage } from "./AgentChangesPage";

export const feature: Feature = { id: "agent-changes", title: { zh: "agent 变更", en: "Agent changes" }, path: "agent-changes", order: 0, section: "agents", element: <AgentChangesPage /> };
