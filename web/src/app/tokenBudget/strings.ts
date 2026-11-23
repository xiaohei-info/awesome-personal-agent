import type { Dict } from "../i18n";

// 资料上限的共用文案：估算系数那句话、范围、模型提示只写这一份，两个设置页都引用。
const zh = {
  count: (k: number) => `按约 ${k} 个字符 ≈ 1 token 估算（不调用 tokenizer）`,
  range: (def: number, chars: number, min: number, max: number) => `默认 ${def}（约合原先的 ${chars} 字），可填 ${min} 到 ${max}`,
  bad: (min: number, max: number) => `资料上限填 ${min} 到 ${max} 之间的整数（token）`,
  model: (name: string) => `当前写作模型：${name}`,
  noModel: "还没有配置写作模型",
  fit: "这个数要装得进该模型的上下文窗口",
  over: "超出会让那次调用失败，并按原有的失败处理重试",
};
const en: typeof zh = {
  count: (k) => `Estimated at about ${k} characters per token (no tokenizer is run)`,
  range: (def, chars, min, max) => `Default ${def} (about the former ${chars} characters); allowed ${min} to ${max}`,
  bad: (min, max) => `Use a whole number from ${min} to ${max} for the material limit (tokens)`,
  model: (name) => `Writing model in use: ${name}`,
  noModel: "No writing model is configured yet",
  fit: "The number must fit that model's context window",
  over: "Beyond it the call fails and is retried the way failures already are",
};
export const strings: Dict<typeof zh> = { zh, en };
