import type { Feature } from "../../app/registry";
import { AgentChangesPage } from "./AgentChangesPage";

export const feature: Feature = { id: "agent-changes", title: "agent 变更", path: "agent-changes", order: 0, section: "agents", element: <AgentChangesPage /> };
