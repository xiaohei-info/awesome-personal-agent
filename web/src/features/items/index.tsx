import type { Feature } from "../../app/registry";
import { ItemsPage } from "./ItemsPage";

// 原「能力包视图」并进事项列表的类型筛选（设计 §3.2）；旧地址 /pack-views 改写到这里。
export const feature: Feature = { id: "items", title: { zh: "事项", en: "Items" }, path: "items", order: 0, section: "items", aliases: ["pack-views"], element: <ItemsPage /> };
