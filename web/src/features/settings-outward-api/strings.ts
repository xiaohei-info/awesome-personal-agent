import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 设置 › 对外接口：外部程序来向 APA 要数据用的钥匙与接口说明（§4.1、§14.2）。明文只在签发时显示一次。
// 与「提醒渠道」分清：这里是外部来向 APA 要；渠道是 APA 把提醒推给外部并接收回答。
// 接口说明只写已实现的路由与参数（附录 B.2、B.3）。
// 方法说明每条一句；路径、参数、示例与返回是代码，在页面文件里（不分语言）。
const zhMethods = [
  { name: "读事项（导出）", what: "全部事项，按参数筛选与排序" },
  { name: "读单个事项", what: "全文、进展、关联与关联键" },
  { name: "读材料", what: "材料登记，可按项目筛" },
  { name: "读单份材料", what: "登记与当前修订全文；rev 取某次修订" },
  { name: "订阅事件", what: "长轮询拉取事项、材料等的变化，游标由你保存" },
];
const enMethods: typeof zhMethods = [
  { name: "Read items (export)", what: "All items, filtered and sorted by parameters" },
  { name: "Read one item", what: "Full text, progress, relations, keys" },
  { name: "Read materials", what: "Material records, optionally by project" },
  { name: "Read one material", what: "Record and current revision; rev picks one" },
  { name: "Subscribe to events", what: "Long-poll changes to items, materials and more; you keep the cursor" },
];

const zh = {
  ...common.zh,
  title: "对外接口",
  summary: (n: number) => `${n} 把在用`,
  lede: "外部程序来读你的事项、材料，或订阅变化，用这里的钥匙和下面的接口说明。",
  hint: "程序由你自己写，APA 只提供接口和钥匙。",
  notChannel: "要接收提醒并回答的程序，不在这里，去「设置 › 提醒渠道」。",
  issueTitle: "签发一把钥匙",
  label: "备注（可选，只给你自己看）",
  labelHint: "比如：家里的看板脚本",
  onlyEvents: "只允许订阅事件（不能读事项和材料）",
  canAll: "能读事项、材料，能订阅事件",
  canEvents: "只能订阅事件",
  issue: "签发",
  keys: "已签发的钥匙",
  none: "还没有钥匙",
  revoked: "已停用",
  revoke: "停用",
  secretOnce: "只显示这一次，请立即保存：",
  savedIt: "我已保存",
  docTitle: "接口说明",
  docBase: "基址",
  docAuth: "鉴权",
  docKey: "上面签发的钥匙",
  docReadOnly: "全部只读，返回 JSON。钥匙放在请求头里，不要放在地址里。",
  methods: zhMethods,
  params: "可选参数",
  eventNotes: ["cursor 从 0 开始；整页处理完再保存返回的 next", "wait 最多 60 秒，有新事件立即返回", "至少一次投递：按 seq 去重", "信封只有元信息，正文按 object 再读"],
  docErrors: "出错时",
  errors: [
    ["400 invalid_request", "参数不对：按 why 与 next 改参数再试"],
    ["401 unauthenticated", "没带钥匙、钥匙不对或已停用：检查请求头"],
    ["403 permission_denied", "这把钥匙不能做这件事：只能订阅事件的钥匙读了事项或材料"],
    ["404 not_found", "对象不存在或已删除"],
  ] as [string, string][],
};

const en: typeof zh = {
  ...common.en,
  title: "Outward API",
  summary: (n) => `${n} in use`,
  lede: "Keys and interface notes for outside programs that read your items and materials or subscribe to changes.",
  hint: "You write the program; APA only provides the interface and the key.",
  notChannel: "A program that receives reminders and answers is not here: see Settings › Reminder channels.",
  issueTitle: "Issue a key",
  label: "Note (optional, only for you)",
  labelHint: "e.g. the dashboard script at home",
  onlyEvents: "Only subscribe to events (cannot read items or materials)",
  canAll: "Reads items and materials, subscribes to events",
  canEvents: "Subscribes to events only",
  issue: "Issue",
  keys: "Issued keys",
  none: "No keys yet",
  revoked: "Revoked",
  revoke: "Revoke",
  secretOnce: "Shown only once. Save it now:",
  savedIt: "I've saved it",
  docTitle: "Interface",
  docBase: "Base URL",
  docAuth: "Auth",
  docKey: "the key issued above",
  docReadOnly: "Everything is read-only JSON. Send the key in the header, never in the URL.",
  methods: enMethods,
  params: "Optional parameters",
  eventNotes: ["cursor starts at 0; save next after the whole page", "wait up to 60 s; returns on news", "At least once: dedupe by seq", "Envelopes are metadata; read by object"],
  docErrors: "Errors",
  errors: [
    ["400 invalid_request", "Bad parameters: fix them per why and next"],
    ["401 unauthenticated", "Missing, wrong or revoked key: check the header"],
    ["403 permission_denied", "This key may not do that: an events-only key read items or materials"],
    ["404 not_found", "The object does not exist or was deleted"],
  ],
};

export const strings: Dict<typeof zh> = { zh, en };
