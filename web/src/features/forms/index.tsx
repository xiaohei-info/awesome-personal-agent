import { Navigate } from "react-router-dom";
import type { Feature } from "../../app/registry";

// 一次性表单已取消（设计 §14.3）：旧链接只是导航，落到需要登录的「等你决定」；令牌不再能回答任何问题。
export const feature: Feature = { id: "forms", title: { zh: "一次性表单", en: "One-time form" }, path: "forms", public: true, element: <Navigate replace to="/decisions" /> };
