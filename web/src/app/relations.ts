// 事项指向材料的内置关系类型（docs/solution-design.md §6.3）；其余类型（开放字符串）原样显示。
export const MATERIAL_RELATION_LABELS: Record<string, string> = { basis: "依据", output: "产出", minutes: "纪要" };

// 材料 id 由内核以 mat_ 前缀生成（kernel/materials newID）；关系的另一端据此区分事项与材料。
export const isMaterialId = (id: string) => id.startsWith("mat_");
