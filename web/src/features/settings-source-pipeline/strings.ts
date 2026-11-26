import type { Dict } from "../../app/i18n";
import { shared } from "../settings-policy/strings";

// 设置 › 来源处理（§9.3）：执行 Agent、队列参数、结果提醒、两份提示词、高级（重试/每日上限/不确定带）。
const zh = {
  ...shared.zh,
  title: "来源处理",
  lede: "新消息进来后，由谁处理、多久处理一批、怎么判断和怎么汇报。",
  summary: (batch: number, wait: number, urgent: boolean) =>
    `攒够 ${batch} 条或等 ${wait} 分钟` + (urgent ? " · 紧急马上处理" : ""),

  agentTitle: "谁来处理",
  agentHint: "它能做什么看它自己的权限。",
  agentLabel: "处理的 Agent",

  scheduleTitle: "多久处理一次",
  scheduleHint: "新消息进队列后，满条数或到时间就会处理一批。",
  batchLabel: "攒够几条就处理",
  batchUnit: "条",
  waitLabel: "最早一条等几分钟就处理",
  waitUnit: "分钟",
  urgentLabel: "紧急的马上处理",
  urgentOn: "开",
  urgentOff: "关",
  maxBatchLabel: "一批最多处理几条",
  batchBad: "攒批条数要是 1 到 100 的整数",
  waitBad: "等待分钟数要是 1 到 1440 的整数",
  maxBatchBad: "每批最多条数要是 1 到 100 的整数",

  judgeTitle: "判断标准（哪些值得处理、哪些算紧急）",
  judgeHint: "留空用内置默认；修改后只影响之后新进来的消息。",
  batchPromptTitle: "给处理 Agent 的说明",
  batchPromptHint: "留空用内置默认；修改后从下一批开始生效。",
  promptPlaceholder: "留空用内置默认",
  promptLabel: "补充或自定义说明",
  promptTooLong: (max: number) => `最多 ${max} 个字`,
  viewBuiltin: "查看内置默认",
  builtinLoading: "正在读取内置默认……",
  builtinFailed: "内置默认没读到，稍后再试。",
  resetDefault: "恢复默认",

  advanced: "高级",
  retryTitle: "重试与每日上限",
  retriesLabel: "失败后最多重试（次）",
  dailyLabel: "每天最多处理（批，0 表示不限）",
  retriesBad: "重试次数要是 0 到 10 的整数",
  dailyBad: "每天批数上限要是 0 到 1000 的整数",

  bandTitle: "拿不准的范围",
  bandHint: "判断得分落在这个区间内时按需要处理，留待复核。",
  bandLow: "下限",
  bandHigh: "上限",
  bandBad: "填 0 到 1 之间的两个数，且下限不大于上限",
  bandDefault: "（当前用内置默认）",
};

const en: typeof zh = {
  ...shared.en,
  title: "Source handling",
  lede: "Who handles incoming messages, how often batches run, what counts as worth handling, and how results are shared.",
  summary: (batch, wait, urgent) =>
    `Every ${batch} messages or ${wait} min` + (urgent ? " · urgent right away" : ""),

  agentTitle: "Who handles them",
  agentHint: "What it can do depends on its own permissions.",
  agentLabel: "Handling Agent",

  scheduleTitle: "How often to run",
  scheduleHint: "Queued messages run as a batch when either count or wait time is reached.",
  batchLabel: "Run once queued messages reach",
  batchUnit: "messages",
  waitLabel: "Or oldest message has waited",
  waitUnit: "minutes",
  urgentLabel: "Handle urgent messages right away",
  urgentOn: "On",
  urgentOff: "Off",
  maxBatchLabel: "At most per batch",
  batchBad: "Batch size must be an integer from 1 to 100",
  waitBad: "Wait minutes must be an integer from 1 to 1440",
  maxBatchBad: "Max per batch must be an integer from 1 to 100",

  judgeTitle: "Screening criteria (worth handling, urgent)",
  judgeHint: "Leave empty to use the built-in default; applies to new messages going forward.",
  batchPromptTitle: "Instructions for the handling Agent",
  batchPromptHint: "Leave empty to use the built-in default; applies from the next batch.",
  promptPlaceholder: "Leave empty for built-in default",
  promptLabel: "Custom instructions",
  promptTooLong: (max) => `At most ${max} characters`,
  viewBuiltin: "View built-in default",
  builtinLoading: "Loading built-in default…",
  builtinFailed: "Could not load the built-in default.",
  resetDefault: "Use default",

  advanced: "Advanced",
  retryTitle: "Retries and daily limit",
  retriesLabel: "Max retries after failure",
  dailyLabel: "Max batches per day (0 = no limit)",
  retriesBad: "Retries must be an integer from 0 to 10",
  dailyBad: "Daily batch limit must be an integer from 0 to 1000",

  bandTitle: "Uncertain score band",
  bandHint: "Scores inside this range are kept for handling rather than dropped.",
  bandLow: "Low",
  bandHigh: "High",
  bandBad: "Enter two numbers between 0 and 1, with low <= high",
  bandDefault: " (built-in default)",
};

export const strings: Dict<typeof zh> = { zh, en };
