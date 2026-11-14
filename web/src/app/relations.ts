
// 材料 id 由内核以 mat_ 前缀生成（kernel/materials newID）；关系的另一端据此区分事项与材料。
export const isMaterialId = (id: string) => id.startsWith("mat_");
