// 外壳与各页共用的文案。英文用词与 API、CLI 已有的英文名一致，术语表：
//
// | 中文 | English | 中文 | English |
// |---|---|---|---|
// | 事项 | item | 决策 / 决策待办 | decision / Decisions |
// | 建议 / 建议流 | proposal / Proposals | 来源 | source |
// | 外发范围 | outbound scope | 判断 / 判断记录 | judgment / judgment record |
// | 判断端点 | judge endpoint | 生成端点 | generate endpoint |
// | 不确定带 | uncertainty band | 影子模式 | shadow mode |
// | 抽检 | spot check | 授权单 | grant |
// | 执行者 | executor | 能力包 | capability pack |
// | 简报 | brief | 知会 | digest |
// | 积压 | backlog | 委派 / 任务委派 | delegation / Delegations |
// | 代答 | answer on behalf | 人格 | persona |
// | 打扰策略 | interruption policy | 材料 | material |
// | 规范 | guideline | 项目 | project |
// | 运行记录 | runs | 进展 | progress |
// | 计划日期 | planned date | 久未推进 | stale |
// | 本人 | owner (界面里多用 you) | 物理删除 | permanent deletion |
// | web_only、observe、auto 等枚举 | 原样保留 | | |
import type { Dict, Lang } from "./i18n";

const zh = {
  brand: "事情交给 Agent，决定留给你",
  loading: "加载中…",
  retry: "重试",
  save: "保存",
  cancel: "取消",
  close: "关闭",
  // 保存成功只说「已保存」；版本号只在显示原始值里出现（界面设计 §6.2）。
  saved: (_v?: number) => "已保存",
  unsaved: "未保存",
  leaveUnsaved: "有改动还没保存，确定离开？",
  backTo: "返回刚才的页面",
  sessionError: (e: string) => `无法确认登录状态：${e}`,
  noPages: "暂无可用页面",
  colon: "：",
  listSep: "、",
  // 导航
  navLabel: "主导航",
  more: "更多",
  // 导航底部的状态灯（点开进诊断 › 状态）
  statusOk: "运行正常",
  statusPaused: "模型已暂停",
  statusNeeds: (n: number) => `${n} 件要处理`,
  statusNotes: (n: number) => `运行正常 · ${n} 条提示`,
  allPages: "全部页面",
  tabs: (entry: string) => `${entry}页签`,
  theme: "明 / 暗",
  themeLabel: "切换明暗主题",
  logout: "退出登录",
  jump: "跳转到…",
  jumpLabel: "跳转到页面",
  jumpDialog: "跳转",
  jumpPlaceholder: "跳转到页面…",
  jumpList: "页面",
  jumpEmpty: "没有匹配的页面",
  jumpSelect: "选择",
  jumpOpen: "打开",
  jumpClose: "关闭",
  searchItemPlaceholder: "搜索项目或事项…",
  itemSearchEmpty: "没有匹配的事项",
  clearChoice: "清空",
  // 登录
  loginPitch1: "事情交给 Agent，",
  loginPitch2: "决定",
  loginPitch3: "留给你。",
  loginSub: "Agent 替你读邮件、看日历、推进事项。遇到该你拍板的事，它会停下来，在这里等你一句话。",
  login: "登录",
  password: "口令",
  cannotConnect: "无法连接服务",
  reauthTitle: "再输一次口令",
  reauthWhy: "发钥匙、填凭据、改工具权限这类事，要再确认一次是你",
  reauthGo: "确认",
  language: "界面语言",
  details: "详情",
  // 事项指向材料的内置关系类型（§6.3）；其余类型原样显示
  // 积压在条件满足后会不会自动判断，随运行配置 unjudged_rejudge_days（§9.3）：0 时关闭自动重判，要在积压里放开。
  rejudgeBack: (days: number) => (days > 0 ? `恢复后自动判断（最近 ${days} 天的）` : "恢复后在积压里放开"),
  rejudgeChosen: (days: number) => (days > 0 ? `选好后自动判断（最近 ${days} 天的）` : "选好后在积压里放开"),
  relation: { basis: "依据", output: "产出", minutes: "纪要" } as Record<string, string>,
};

const en: typeof zh = {
  brand: "Hand the work to your Agent, keep the decisions",
  loading: "Loading…",
  retry: "Retry",
  save: "Save",
  cancel: "Cancel",
  close: "Close",
  saved: () => "Saved",
  unsaved: "Not saved",
  leaveUnsaved: "You have unsaved changes. Leave anyway?",
  backTo: "Back to where you were",
  sessionError: (e) => `Could not check the sign-in state: ${e}`,
  noPages: "No pages available",
  colon: ": ",
  listSep: ", ",
  navLabel: "Main navigation",
  more: "More",
  statusOk: "All good",
  statusPaused: "Models paused",
  statusNeeds: (n: number) => `${n} need${n === 1 ? "s" : ""} you`,
  statusNotes: (n: number) => `All good · ${n} note${n === 1 ? "" : "s"}`,
  allPages: "All pages",
  tabs: (entry) => `${entry} tabs`,
  theme: "Light / Dark",
  themeLabel: "Toggle light and dark theme",
  logout: "Sign out",
  jump: "Jump to…",
  jumpLabel: "Jump to page",
  jumpDialog: "Jump",
  jumpPlaceholder: "Jump to page…",
  jumpList: "Pages",
  jumpEmpty: "No matching page",
  jumpSelect: "select",
  jumpOpen: "open",
  jumpClose: "close",
  searchItemPlaceholder: "Search project or item…",
  itemSearchEmpty: "No matching items",
  clearChoice: "Clear",
  loginPitch1: "Hand the work to your Agent,",
  loginPitch2: "keep the decisions",
  loginPitch3: " yourself.",
  loginSub: "Agents read your mail, watch your calendar and move items forward. When something needs your call, they stop and wait for you here.",
  login: "Sign in",
  password: "Password",
  cannotConnect: "Cannot reach the server",
  reauthTitle: "Enter your password again",
  reauthWhy: "Issuing keys, entering credentials or changing tool access needs one more check that it is you",
  reauthGo: "Confirm",
  language: "Interface language",
  details: "Details",
  rejudgeBack: (days) => (days > 0 ? `Judged automatically once back (last ${days} days)` : "Release from the backlog once back"),
  rejudgeChosen: (days) => (days > 0 ? `Judged automatically once chosen (last ${days} days)` : "Release from the backlog once chosen"),
  relation: { basis: "basis", output: "output", minutes: "minutes" },
};

export const common: Dict<typeof zh> = { zh, en };

/** 积压原因在条件满足后的去向；days 取自 GET /health 的 unjudged_backlog.detail.rejudge_days，取不到时不说。其余原因不自动判断，见词表。 */
export function rejudgeHint(lang: Lang, reason: string, days: number | undefined): string | undefined {
  if (days === undefined) return undefined;
  if (reason === "outbound_unset") return common[lang].rejudgeChosen(days);
  if (reason === "model_unavailable" || reason === "models_paused") return common[lang].rejudgeBack(days);
  return undefined;
}

// 不随界面语言变的固定写法：品牌字形与各语言的自称（语言选项总用该语言自己的名字）。
export const BRAND_MARK = "决";
export const LANG_NAMES = { zh: "中文", en: "English" } as const;
