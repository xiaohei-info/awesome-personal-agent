// 资料预算（token）的唯一一份网页端常量：写作（设置 › 模型）使用。
// 与内核 kernel/policy 同值：CharsPerToken、DefaultGenerateBudgetTokens、Min/MaxBudgetTokens；
// 内核与网页是两种语言，各存一份，由 kernel/policy 的 TestWebBudgetConstantsMatchKernel 锁住同值。
export const CHARS_PER_TOKEN = 1.5;
export const DEFAULT_BUDGET_TOKENS = 16000;
export const MIN_BUDGET_TOKENS = 2000;
export const MAX_BUDGET_TOKENS = 500000;

/** 页面先按服务端同一规则挡一次：整数且在范围内（0、负数、过小、过大都不放行，不截到边界）。 */
export const budgetOk = (v: string): boolean => {
  const n = Number(v);
  return v.trim() !== "" && Number.isInteger(n) && n >= MIN_BUDGET_TOKENS && n <= MAX_BUDGET_TOKENS;
};
