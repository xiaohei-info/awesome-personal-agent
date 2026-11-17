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
  withEvents: "连同它的来源事件一起删除",
  warnEvents: "挂在它上面的来源事件（原文、取回的附件与判断记录）也会一并删除。",
  sourceEventsGone: (n: number) => `连同删除了 ${n} 条来源事件`,
  eventDelete: "删除这条来源事件…",
  eventWarn1: "永久删除来源事件「",
  eventWarn2: "」的原文、取回的附件与判断记录，无法找回；同一事件再被来源带来时不会重新入账。",
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
  withEvents: "Also delete its source events",
  warnEvents: "The source events attached to it (original text, fetched attachments and judgment records) are deleted too.",
  sourceEventsGone: (n) => `${n} source events deleted with it`,
  eventDelete: "Delete this source event…",
  eventWarn1: "Permanently delete the original text, fetched attachments and judgment records of source event \"",
  eventWarn2: "\"? It cannot be recovered; if the source brings the same event again it will not be taken in.",
};

export const strings: Dict<typeof zh> = { zh, en };
