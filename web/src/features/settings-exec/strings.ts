import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "执行者与声明",
  lede: "谁能被派活、各 harness 声明了哪些可在本机执行的命令。声明要你确认才生效。",
  executors: "执行者清单",
  executorId: "执行者 id",
  kind: "种类",
  host: "主机",
  scenarios: "场景（逗号分隔）",
  add: "添加",
  noExecutors: "还没有执行者",
  remove: "删除",
  pending: "待确认声明",
  pendingHint: "确认前声明不生效；确认意味着接受下列声明的全部内容，其中的命令将可在本机执行。",
  noPending: "没有待确认的声明",
  confirm: "确认",
  reject: "拒绝",
  badJSON: "声明不是合法的 JSON",
  commands: "可执行命令",
  declaration: "声明（JSON）",
  register: "登记",
  noCommands: "还没有可执行命令",
};

const en: typeof zh = {
  ...common.en,
  title: "Executors & declarations",
  lede: "Who can be given work, and which commands each harness declares it can run on this machine. A declaration takes effect only after you confirm it.",
  executors: "Executors",
  executorId: "Executor id",
  kind: "Kind",
  host: "Host",
  scenarios: "Scenarios (comma-separated)",
  add: "Add",
  noExecutors: "No executors yet",
  remove: "Delete",
  pending: "Declarations to confirm",
  pendingHint: "A declaration has no effect until confirmed; confirming accepts everything in it, and its commands become runnable on this machine.",
  noPending: "No declarations waiting for confirmation",
  confirm: "Confirm",
  reject: "Reject",
  badJSON: "The declaration is not valid JSON",
  commands: "Runnable commands",
  declaration: "Declaration (JSON)",
  register: "Register",
  noCommands: "No runnable commands yet",
};

export const strings: Dict<typeof zh> = { zh, en };
