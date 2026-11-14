import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 设置 › 永久删除：彻底删掉一条事项或材料的内容，只留下「这里有过一条记录」（§6.15）。
const zh = {
  ...common.zh,
  title: "永久删除",
  done: (name: string) => `已永久删除「${name}」`,
  details: "详情",
  detailEvents: (n: number) => `${n} 条相关记录只留下时间和类型`,
  blobsRemoved: (b: string) => `已删除的附件：${b}`,
  blobsRetained: (b: string) => `仍被其他材料引用、保留的附件：${b}`,
  objectId: "要删除的事项或材料（标题或编号）",
  find: "查找",
  candidates: "找到的事项与材料",
  item: "事项",
  material: "材料",
  noHits: "没找到，可直接按编号删除",
  delete: "删除…",
  warn1: "永久删除「",
  warn2: "」，无法找回；只留下「这里有过一条记录」。",
  confirm: "确认永久删除",
};

const en: typeof zh = {
  ...common.en,
  title: "Permanent deletion",
  done: (name) => `"${name}" was deleted permanently`,
  details: "Details",
  detailEvents: (n) => `${n} related records keep only their time and type`,
  blobsRemoved: (b) => `Attachments deleted: ${b}`,
  blobsRetained: (b) => `Attachments kept because other materials use them: ${b}`,
  objectId: "Item or material to delete (title or ID)",
  find: "Find",
  candidates: "Matching items and materials",
  item: "Item",
  material: "Material",
  noHits: "Nothing found; you can still delete by ID",
  delete: "Delete…",
  warn1: "Permanently delete \"",
  warn2: "\"? It cannot be recovered; only a note that something was here remains.",
  confirm: "Delete permanently",
};

export const strings: Dict<typeof zh> = { zh, en };
