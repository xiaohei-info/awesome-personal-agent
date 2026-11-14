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
import type { Dict } from "./i18n";

const zh = {
  brand: "事情交给 agent，决定留给你",
  loading: "加载中…",
  retry: "重试",
  save: "保存",
  cancel: "取消",
  close: "关闭",
  saved: (v: number) => `已保存（版本 ${v}）`,
  sessionError: (e: string) => `无法确认登录状态：${e}`,
  noPages: "暂无可用页面",
  colon: "：",
  listSep: "、",
  // 导航
  navLabel: "主导航",
  more: "更多",
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
  // 登录
  loginPitch1: "事情交给 agent，",
  loginPitch2: "决定",
  loginPitch3: "留给你。",
  loginSub: "agent 替你读邮件、看日历、推进事项。遇到该你拍板的事，它会停下来，在这里等你一句话。",
  login: "登录",
  password: "口令",
  cannotConnect: "无法连接服务",
  language: "界面语言",
  details: "详情",
  // 事项指向材料的内置关系类型（§6.3）；其余类型原样显示
  relation: { basis: "依据", output: "产出", minutes: "纪要" } as Record<string, string>,
};

const en: typeof zh = {
  brand: "Hand the work to agents, keep the decisions",
  loading: "Loading…",
  retry: "Retry",
  save: "Save",
  cancel: "Cancel",
  close: "Close",
  saved: (v) => `Saved (version ${v})`,
  sessionError: (e) => `Could not check the sign-in state: ${e}`,
  noPages: "No pages available",
  colon: ": ",
  listSep: ", ",
  navLabel: "Main navigation",
  more: "More",
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
  loginPitch1: "Hand the work to agents,",
  loginPitch2: "keep the decisions",
  loginPitch3: " yourself.",
  loginSub: "Agents read your mail, watch your calendar and move items forward. When something needs your call, they stop and wait for you here.",
  login: "Sign in",
  password: "Password",
  cannotConnect: "Cannot reach the server",
  language: "Interface language",
  details: "Details",
  relation: { basis: "basis", output: "output", minutes: "minutes" },
};

export const common: Dict<typeof zh> = { zh, en };

// 不随界面语言变的固定写法：品牌字形与各语言的自称（语言选项总用该语言自己的名字）。
export const BRAND_MARK = "决";
export const LANG_NAMES = { zh: "中文", en: "English" } as const;
