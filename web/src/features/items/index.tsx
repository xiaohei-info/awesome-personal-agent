import type { Feature } from "../../app/registry";
import { ItemsPage, NewItemPage } from "./ItemsPage";

// 原「能力包视图」并进事项列表的类型筛选（设计 §3.2）；旧地址 /pack-views 改写到这里。
export const feature: Feature = { id: "items", title: { zh: "事项", en: "Items" }, path: "items", order: 0, section: "items", aliases: ["pack-views"], element: <ItemsPage /> };
// 预填新建页的短地址 /new（§4.1）：只作链接目标，不进导航。
export const newItem: Feature = { id: "items-new", title: { zh: "新建事项", en: "New item" }, path: "new", hidden: true, element: <NewItemPage /> };
export const features: Feature[] = [feature, newItem];
