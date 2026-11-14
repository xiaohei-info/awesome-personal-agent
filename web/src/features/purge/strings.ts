import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "物理删除",
  lede: "彻底删除一个事项或材料的内容：投影、正文和无其他引用的 blob 被删除，相关事件的内容替换为墓碑（序号、类型、时间、actor 保留）。",
  done: (kind: string, obj: string, n: number) => `已删除 ${kind} ${obj}：${n} 条事件已墓碑化`,
  blobsRemoved: (b: string) => `；已删除 blob：${b}`,
  blobsRetained: (b: string) => `；仍被其他材料引用而保留的 blob：${b}`,
  objectId: "对象 id",
  delete: "删除…",
  warn1: "将永久删除 ",
  warn2: " 的全部内容。此操作不可恢复，删除后无法找回。",
  confirm: "确认永久删除",
};

const en: typeof zh = {
  ...common.en,
  title: "Permanent deletion",
  lede: "Delete the content of an item or material for good: the projection, the body and blobs no longer referenced are removed, and the related events are replaced by tombstones (sequence, type, time and actor are kept).",
  done: (kind, obj, n) => `Deleted ${kind} ${obj}: ${n} event${n === 1 ? "" : "s"} tombstoned`,
  blobsRemoved: (b) => `; blobs removed: ${b}`,
  blobsRetained: (b) => `; blobs kept because other materials still reference them: ${b}`,
  objectId: "Object id",
  delete: "Delete…",
  warn1: "This permanently deletes all content of ",
  warn2: ". It cannot be undone or recovered.",
  confirm: "Delete permanently",
};

export const strings: Dict<typeof zh> = { zh, en };
