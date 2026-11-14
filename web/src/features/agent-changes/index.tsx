import type { Feature } from "../../app/registry";
import { AgentChangesPage } from "./AgentChangesPage";

export const feature: Feature = { id: "agent-changes", title: { zh: "改动", en: "Changes" }, path: "agent-changes", order: 0, section: "agents", element: <AgentChangesPage /> };
