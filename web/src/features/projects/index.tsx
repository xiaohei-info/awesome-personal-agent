import type { Feature } from "../../app/registry";
import { ProjectsPage } from "./ProjectsPage";

export const feature: Feature = { id: "projects", title: { zh: "项目", en: "Projects" }, path: "projects", order: 0, section: "projects", element: <ProjectsPage /> };
