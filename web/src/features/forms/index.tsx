import type { Feature } from "../../app/registry";
import { FormPage } from "./FormPage";

// 一次性表单：令牌即凭据，不登录；public 页不进导航。
export const feature: Feature = { id: "forms", title: { zh: "一次性表单", en: "One-time form" }, path: "forms", public: true, element: <FormPage /> };
