import type { Feature } from "../../app/registry";
import { ProjectsPage } from "./ProjectsPage";

export const feature: Feature = { id: "projects", title: "项目", path: "projects", order: 11, group: "工作", element: <ProjectsPage /> };
