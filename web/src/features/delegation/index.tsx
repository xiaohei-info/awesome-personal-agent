import type { Feature } from "../../app/registry";
import { DelegationPage } from "./DelegationPage";

export const feature: Feature = { id: "delegation", title: { zh: "委派", en: "Delegations" }, path: "delegation", order: 1, section: "agents", element: <DelegationPage /> };
