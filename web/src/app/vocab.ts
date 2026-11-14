import type { Dict } from "./i18n";
import type * as I from "./ids";

// 取值词表（设计 §4）：内核 ID → 界面上的名字（label）与可选的一短句（hint）。
// 封闭集合用 Record：ids.ts 里多一个 ID 而这里没写，tsc 失败。开放集合用 Open：查不到时显示原值。
export interface Entry {
  label: string;
  hint?: string;
}
type Closed<T extends readonly string[]> = Record<T[number], Entry>;
type Open = Partial<Record<string, Entry>>;

export interface Vocab {
  phase: Closed<typeof I.PHASE>;
  priority: Closed<typeof I.PRIORITY>;
  itemKind: Closed<typeof I.ITEM_KIND>;
  waiting: Closed<typeof I.WAITING>;
  relation: Closed<typeof I.RELATION>;
  projectStatus: Closed<typeof I.PROJECT_STATUS>;
  deliveryClass: Closed<typeof I.DELIVERY_CLASS>;
  deliveryState: Closed<typeof I.DELIVERY_STATE>;
  route: Closed<typeof I.ROUTE>;
  suppressed: Closed<typeof I.SUPPRESSED>;
  runCategory: Closed<typeof I.RUN_CATEGORY>;
  runState: Closed<typeof I.RUN_STATE>;
  runner: Closed<typeof I.RUNNER>;
  launch: Closed<typeof I.LAUNCH>;
  outcome: Closed<typeof I.OUTCOME>;
  actorType: Closed<typeof I.ACTOR_TYPE>;
  via: Closed<typeof I.VIA>;
  sessionKind: Closed<typeof I.SESSION_KIND>;
  error: Closed<typeof I.ERROR_CODE>;
  sourceType: Closed<typeof I.SOURCE_TYPE>;
  disposition: Closed<typeof I.DISPOSITION>;
  unjudgedReason: Closed<typeof I.UNJUDGED_REASON>;
  outboundScope: Closed<typeof I.OUTBOUND_SCOPE>;
  backlogStep: Closed<typeof I.BACKLOG_STEP>;
  attachState: Closed<typeof I.ATTACH_STATE>;
  webOnly: Closed<typeof I.WEB_ONLY>;
  perm: Closed<typeof I.PERM>;
  write: Closed<typeof I.WRITE>;
  muteScope: Closed<typeof I.MUTE_SCOPE>;
  locality: Closed<typeof I.LOCALITY>;
  protocol: Closed<typeof I.PROTOCOL>;
  probSource: Closed<typeof I.PROB_SOURCE>;
  grantAction: Closed<typeof I.GRANT_ACTION>;
  grantStatus: Closed<typeof I.GRANT_STATUS>;
  credKind: Closed<typeof I.CRED_KIND>;
  executorKind: Closed<typeof I.EXECUTOR_KIND>;
  ruleMode: Closed<typeof I.RULE_MODE>;
  ruleGroup: Closed<typeof I.RULE_GROUP>;
  ruleClass: Closed<typeof I.RULE_CLASS>;
  judgeUse: Closed<typeof I.JUDGE_USE>;
  judgeTemplate: Closed<typeof I.JUDGE_TEMPLATE>;
  judgeBackend: Closed<typeof I.JUDGE_BACKEND>;
  decidedBy: Closed<typeof I.DECIDED_BY>;
  levelOutcome: Closed<typeof I.LEVEL_OUTCOME>;
  spotResult: Closed<typeof I.SPOT_RESULT>;
  healthSignal: Closed<typeof I.HEALTH_SIGNAL>;
  doctorCheck: Closed<typeof I.DOCTOR_CHECK>;
  event: Closed<typeof I.EVENT> & Closed<typeof I.EVENT_INTERNAL>;
  materialKind: Closed<typeof I.MATERIAL_KIND> & Open;
  pack: Closed<typeof I.PACK> & Open;
  rule: Closed<typeof I.BUILTIN_RULE> & Open;
  viewField: Closed<typeof I.VIEW_FIELD> & Open;
}
export type VocabKey = keyof Vocab;

/** 渠道路由 channel:<名> 的显示名。 */
export const CHANNEL_PREFIX = "channel:";

const zh: Vocab & { channelRoute: (name: string) => string } = {
  phase: { proposed: { label: "建议" }, open: { label: "进行中" }, waiting: { label: "等待中" }, needs_input: { label: "等你决定" }, done: { label: "已完成" }, dropped: { label: "已放弃" } },
  priority: { low: { label: "低" }, normal: { label: "普通" }, high: { label: "高" }, urgent: { label: "紧急" } },
  itemKind: { task: { label: "任务" }, commitment: { label: "承诺" }, decision: { label: "决策" }, delegation: { label: "委派" }, brief: { label: "简报" }, routine: { label: "例行任务" } },
  waiting: { human: { label: "等某人" }, executor: { label: "等执行者" }, external: { label: "等外部回复" }, time: { label: "等到某个时间" } },
  relation: { relates: { label: "相关" }, parent: { label: "上级" }, blocks: { label: "阻塞" }, derived_from: { label: "由此派生" }, supersedes: { label: "取代" }, basis: { label: "依据" }, output: { label: "产出" }, minutes: { label: "纪要" } },
  projectStatus: { active: { label: "进行中" }, paused: { label: "暂停" }, archived: { label: "已归档" } },
  deliveryClass: { decision: { label: "决策" }, brief: { label: "简报" }, reminder: { label: "提醒" }, health: { label: "系统健康" }, wake: { label: "唤醒宿主" } },
  deliveryState: { pending: { label: "待送出" }, claimed: { label: "正在送" }, delivered: { label: "已送达" }, acknowledged: { label: "已确认" }, fallback_web: { label: "留在网页未读" }, superseded: { label: "已作废" } },
  route: { inject: { label: "下次聊天时告诉我" }, wake: { label: "唤醒宿主开工", hint: "不提醒你，只让宿主开一个回合" }, web: { label: "只放网页未读" } },
  suppressed: { daily_cap: { label: "超过今天上限" }, muted: { label: "已暂停提醒" } },
  runCategory: { judge: { label: "打分" }, generate: { label: "写作" }, task: { label: "专属任务" }, background: { label: "后台运行" }, source: { label: "读取来源" }, transform: { label: "转换" }, reader: { label: "读取" } },
  runState: { queued: { label: "排队中" }, running: { label: "运行中" }, succeeded: { label: "成功" }, failed: { label: "失败" }, timeout: { label: "超时" } },
  runner: { task: { label: "专属 agent" }, main_session: { label: "主会话" } },
  launch: { headless: { label: "后台启动" }, wake: { label: "唤醒会话" } },
  outcome: { progress: { label: "进展" }, done: { label: "完成" }, blocked: { label: "卡住了" } },
  actorType: { human: { label: "你" }, agent: { label: "agent" }, system: { label: "系统" } },
  via: { web: { label: "网页" }, cli: { label: "命令行" }, clock: { label: "定时" }, rule: { label: "自动规则" }, source: { label: "来源" }, ext: { label: "扩展程序" } },
  sessionKind: { direct: { label: "私聊" }, group: { label: "多人会话" }, unknown: { label: "未知会话" } },
  error: {
    invalid_request: { label: "请求有误" },
    unauthenticated: { label: "登录已失效，请重新登录" },
    permission_denied: { label: "没有权限做这件事" },
    web_only: { label: "这件事需要你在网页上亲自确认" },
    executor_not_allowed: { label: "这个项目不允许派给该执行者" },
    risk_screening_pending: { label: "正在判断风险，判完前只能你在网页上回答" },
    basis_required: { label: "缺少你的原话或临时授权" },
    session_not_direct: { label: "不是与你的私聊，不能凭原话代答" },
    not_found: { label: "找不到这条记录" },
    conflict: { label: "内容已被改动" },
    login_locked: { label: "尝试次数太多，请稍后再试" },
    internal: { label: "服务出错" },
    models_paused: { label: "模型已暂停，先去设置里恢复" },
  },
  sourceType: { pull: { label: "定时拉取" }, script: { label: "自定义脚本" }, webhook: { label: "外部推送" } },
  disposition: { pending: { label: "处理中" }, attached: { label: "已并入事项" }, proposed: { label: "已成建议" }, ignored: { label: "已跳过" }, unjudged: { label: "还没判断" }, expired: { label: "已过期" }, digest: { label: "只进了简报" }, alerted: { label: "已直接提醒" } },
  unjudgedReason: {
    model_unavailable: { label: "等模型恢复", hint: "恢复后自动判断" },
    outbound_unset: { label: "等你选外发范围", hint: "选好后自动判断" },
    undecided: { label: "等模型恢复", hint: "恢复后自动判断" },
    restored: { label: "已放回，等你放开" },
    models_paused: { label: "模型已暂停", hint: "恢复后自动判断" },
  },
  outboundScope: { full: { label: "全部内容" }, excerpt: { label: "摘要和开头" }, meta: { label: "只有标题" }, none: { label: "不发送" } },
  backlogStep: { rules: { label: "先按规则处理" }, score: { label: "试打分" }, bands: { label: "定自动处理的线" }, rejudge: { label: "放进建议流" }, restore: { label: "找回被忽略的消息" } },
  attachState: { pending: { label: "待转给 agent" }, forwarded: { label: "已转给 agent" }, processed: { label: "已处理" }, handed_back: { label: "已交回给你" }, released: { label: "已交回自动判断" } },
  webOnly: {
    funds: { label: "资金", hint: "涉及真实付款或收款" },
    external_commitment: { label: "对外承诺" },
    irreversible: { label: "不可逆" },
    permission_change: { label: "权限变更" },
    spec_activation: { label: "规范生效" },
  },
  perm: { read: { label: "只看" }, edit: { label: "可改文件" }, full: { label: "完全放开", hint: "宿主不再逐项询问" } },
  write: { direct: { label: "直接生效" }, proposed: { label: "先给我看" } },
  muteScope: { source: { label: "来源" }, project: { label: "项目" }, item: { label: "事项" } },
  locality: { local: { label: "我自己的机器" }, external: { label: "外部服务" } },
  protocol: { "system-one": { label: "原生判断接口" }, "probability-judgment/1": { label: "自建分类服务" } },
  probSource: { native: { label: "模型原生概率" }, logprobs: { label: "由对数概率换算" }, self_reported: { label: "模型自报（不可用）" } },
  grantAction: {
    "item.create": { label: "新建事项" },
    "item.update": { label: "修改事项" },
    "item.transition": { label: "改阶段或标完成" },
    "item.relate": { label: "建立关联" },
    "decision.answer": { label: "回答决策" },
    "proposal.accept": { label: "接受建议" },
    "proposal.dismiss": { label: "忽略建议" },
    "proposal.attach": { label: "把建议补到事项" },
    "revision.confirm": { label: "确认文档修改" },
    "revision.reject": { label: "拒绝文档修改" },
    "delegate.report": { label: "替执行者回执" },
    "followmsg.handback": { label: "交回跟进消息" },
  },
  grantStatus: { active: { label: "有效" }, expired: { label: "已过期" }, revoked: { label: "已撤销" } },
  credKind: { observer: { label: "只看" }, worker: { label: "干活的" }, delegate: { label: "我的主助手" }, ext: { label: "扩展程序" }, webhook: { label: "外部推送" } },
  executorKind: { "native-subagent": { label: "宿主里的子 agent" }, "external-cli": { label: "外部命令行工具" }, human: { label: "人" } },
  ruleMode: { off: { label: "关" }, observe: { label: "试运行", hint: "只记录，不动手" }, auto: { label: "开" } },
  ruleGroup: { clock_rules: { label: "定时" }, event_rules: { label: "事件触发" } },
  ruleClass: { reminder: { label: "提醒我" }, action: { label: "替我动手" } },
  judgeUse: {
    inbound_screen: { label: "来源消息值不值得看" },
    capture_prescreen: { label: "聊天里有没有要记的事" },
    risk_screen: { label: "要不要你亲自确认" },
    delivery_defer: { label: "决策能不能晚点送" },
    followup_defer: { label: "提醒能不能晚点提" },
    verify_gate: { label: "回执要不要你核实" },
  },
  judgeTemplate: {
    "triage-screen": { label: "来源消息值不值得看" },
    "capture-prescreen": { label: "聊天里有没有要记的事" },
    "web-only-screening": { label: "要不要你亲自确认" },
    "delivery-defer": { label: "决策能不能晚点送" },
    "follow-up-defer": { label: "提醒能不能晚点提" },
    "delegation-screen": { label: "回执要不要你核实" },
  },
  judgeBackend: { classifier: { label: "打分模型" }, chat: { label: "写作模型" } },
  decidedBy: { classifier: { label: "打分模型" }, chat: { label: "写作模型" }, default: { label: "按保守默认" } },
  levelOutcome: { decided: { label: "已定" }, uncertain: { label: "拿不准" }, error: { label: "出错" }, skipped: { label: "跳过" } },
  spotResult: { should_see: { label: "该看" }, correct: { label: "确实不用" } },
  healthSignal: {
    source_failures: { label: "来源读取失败" },
    outcall: { label: "后台任务排队或超时" },
    delivery_backlog: { label: "提醒发不出去" },
    chain_trips: { label: "自动化连锁被叫停" },
    agentsmd_drift: { label: "仓库规范和 APA 不一致" },
    pack_rejections: { label: "能力包加载失败" },
    model_unavailable: { label: "模型连不上" },
    unjudged_backlog: { label: "积压待处理" },
    rule_fallbacks: { label: "自动规则被退回试运行" },
    receipt_failures: { label: "agent 回报失败" },
    other_alerts: { label: "其他告警" },
    stalled_delegations: { label: "委派久无回音" },
    clock_lag: { label: "定时器迟到" },
  },
  doctorCheck: { data_dir: { label: "数据目录" }, config: { label: "配置文件" }, tier: { label: "运行形态" }, model_endpoint: { label: "模型" }, sources: { label: "来源" }, packs: { label: "能力包" }, host_plugin: { label: "宿主插件" }, overlaps: { label: "重叠检查" }, channels: { label: "聊天渠道" } },
  event: {
    "item.created": { label: "新建了事项" },
    "item.updated": { label: "修改了事项" },
    "item.phase_changed": { label: "改了阶段" },
    "item.deadline_passed": { label: "过了截止" },
    "item.progress_added": { label: "写了进展" },
    "proposal.attached": { label: "把建议补到事项" },
    "decision.requested": { label: "提了一个决策" },
    "decision.resolved": { label: "作出了决定" },
    "delegation.created": { label: "派出了任务" },
    "delegation.reported": { label: "交回了结果" },
    "command.rejected": { label: "操作被拒绝" },
    "material.revised": { label: "改了文档" },
    "material.revision_proposed": { label: "提交了文档修改" },
    "guideline.activated": { label: "规范生效" },
    "project.updated": { label: "修改了项目" },
    "project.allowed_executors_changed": { label: "改了项目可派的执行者" },
    "rule.mode_changed": { label: "切换了自动规则" },
    "allowlist.changed": { label: "改了名单" },
    "policy.changed": { label: "改了设置" },
    "grant.issued": { label: "签发了临时授权" },
    "grant.revoked": { label: "撤销了临时授权" },
    "run.started": { label: "后台运行开始" },
    "run.finished": { label: "后台运行结束" },
    "source.failed": { label: "来源读取失败" },
    "health.alert": { label: "系统告警" },
    "auth.login": { label: "登录" },
    "auth.login_locked": { label: "登录被锁定" },
    "auth.password_changed": { label: "改了口令" },
    "auth.session_revoked": { label: "让会话退出" },
    "rule.observed": { label: "试运行记录" },
    "rule.timer_created": { label: "规则建了定时器" },
    "inbound.disposed": { label: "处理了来源消息" },
    "inbound.requeued": { label: "放回了积压" },
    "inbound.captured": { label: "从聊天里记下" },
    "instance.created": { label: "新建了定时任务" },
    "instance.updated": { label: "修改了定时任务" },
    "instance.toggled": { label: "启停了定时任务" },
    "instance.fired": { label: "定时任务到点" },
    "material.revision_rejected": { label: "拒绝了文档修改" },
    "material.revision_summarized": { label: "写了修改摘要" },
    "followmsg.attached": { label: "收到跟进消息" },
    "followmsg.processed": { label: "处理了跟进消息" },
    "followmsg.handed_back": { label: "交回了跟进消息" },
    "followmsg.released": { label: "跟进消息交回自动判断" },
    "executor.declared": { label: "提交了接入请求" },
    "executor.confirmed": { label: "批准了接入请求" },
    "executor.rejected": { label: "拒绝了接入请求" },
    "executor.changed": { label: "改了执行者" },
    "executor.removed": { label: "删除了执行者" },
    "source.registered": { label: "接入了来源" },
    "source.updated": { label: "修改了来源" },
    "source.enabled": { label: "启用了来源" },
    "source.disabled": { label: "停用了来源" },
    "item.deleted": { label: "永久删除了事项" },
    "run.credential_exchanged": { label: "后台运行领取了钥匙" },
    "timer.fired": { label: "定时器到点" },
  },
  materialKind: { minutes: { label: "纪要" }, reference: { label: "参考资料" }, worknote: { label: "工作笔记" }, requirement: { label: "需求" }, design: { label: "方案" }, guideline: { label: "规范" } },
  pack: {
    brief: { label: "简报" },
    review: { label: "复盘" },
    routine: { label: "例行任务" },
    "follow-up": { label: "跟进提醒" },
    "decision-card": { label: "决策卡片" },
    "background-run": { label: "后台运行" },
    "guideline-draft": { label: "规范起草" },
    "agents-md": { label: "仓库规范文件" },
    "inbound-triage": { label: "来源消息筛选" },
    "delegation-verify": { label: "回执核实" },
  },
  rule: {
    "follow-up/follow-up-interval": { label: "到期没进展时跟进" },
    "follow-up/stale-check": { label: "14 天没动静的事项问我一声" },
    "brief/scheduled-brief": { label: "按时写简报" },
    "review/scheduled-review": { label: "按时写复盘" },
    "routine/scheduled-routine": { label: "按时做例行任务" },
    "guideline-draft/guideline-check": { label: "项目缺规范时起草一份" },
    "background-run/handle-labelled": { label: "带 agent-ok 标签的事项自动开工" },
    "delegation-verify/verify-report": { label: "执行者交回结果时问我要不要核实" },
  },
  viewField: {
    title: { label: "标题" }, project: { label: "项目" }, created_at: { label: "创建时间" }, updated_at: { label: "更新时间" },
    phase: { label: "阶段" }, summary: { label: "摘要" }, tag: { label: "标签" }, tags: { label: "标签" }, deadline: { label: "截止" },
    planned_for: { label: "计划日期" }, priority: { label: "优先级" }, "spec.period": { label: "周期" }, "spec.result": { label: "结果" },
  },
  channelRoute: (name) => `推送到 ${name}`,
};

const en: typeof zh = {
  phase: { proposed: { label: "Proposed" }, open: { label: "Open" }, waiting: { label: "Waiting" }, needs_input: { label: "Needs you" }, done: { label: "Done" }, dropped: { label: "Dropped" } },
  priority: { low: { label: "Low" }, normal: { label: "Normal" }, high: { label: "High" }, urgent: { label: "Urgent" } },
  itemKind: { task: { label: "Task" }, commitment: { label: "Commitment" }, decision: { label: "Decision" }, delegation: { label: "Delegation" }, brief: { label: "Brief" }, routine: { label: "Routine" } },
  waiting: { human: { label: "A person" }, executor: { label: "An executor" }, external: { label: "An outside reply" }, time: { label: "A point in time" } },
  relation: { relates: { label: "Related" }, parent: { label: "Parent" }, blocks: { label: "Blocks" }, derived_from: { label: "Derived from" }, supersedes: { label: "Supersedes" }, basis: { label: "Basis" }, output: { label: "Output" }, minutes: { label: "Minutes" } },
  projectStatus: { active: { label: "Active" }, paused: { label: "Paused" }, archived: { label: "Archived" } },
  deliveryClass: { decision: { label: "Decision" }, brief: { label: "Brief" }, reminder: { label: "Reminder" }, health: { label: "System health" }, wake: { label: "Host wake-up" } },
  deliveryState: { pending: { label: "To send" }, claimed: { label: "Sending" }, delivered: { label: "Delivered" }, acknowledged: { label: "Acknowledged" }, fallback_web: { label: "Left in web unread" }, superseded: { label: "Superseded" } },
  route: { inject: { label: "Tell me next time we chat" }, wake: { label: "Wake the host", hint: "Starts a host turn; does not notify you" }, web: { label: "Web unread only" } },
  suppressed: { daily_cap: { label: "Over today's limit" }, muted: { label: "Reminders paused" } },
  runCategory: { judge: { label: "Scoring" }, generate: { label: "Writing" }, task: { label: "Dedicated task" }, background: { label: "Background run" }, source: { label: "Source read" }, transform: { label: "Transform" }, reader: { label: "Reader" } },
  runState: { queued: { label: "Queued" }, running: { label: "Running" }, succeeded: { label: "Succeeded" }, failed: { label: "Failed" }, timeout: { label: "Timed out" } },
  runner: { task: { label: "Dedicated agent" }, main_session: { label: "Main session" } },
  launch: { headless: { label: "Background start" }, wake: { label: "Session wake-up" } },
  outcome: { progress: { label: "Progress" }, done: { label: "Done" }, blocked: { label: "Blocked" } },
  actorType: { human: { label: "You" }, agent: { label: "agent" }, system: { label: "System" } },
  via: { web: { label: "Web" }, cli: { label: "Command line" }, clock: { label: "Schedule" }, rule: { label: "Automatic rule" }, source: { label: "Source" }, ext: { label: "Extension" } },
  sessionKind: { direct: { label: "Direct chat" }, group: { label: "Group chat" }, unknown: { label: "Unknown chat" } },
  error: {
    invalid_request: { label: "Invalid request" },
    unauthenticated: { label: "Signed out, please sign in again" },
    permission_denied: { label: "Not allowed to do this" },
    web_only: { label: "Only you can confirm this, on the web" },
    executor_not_allowed: { label: "This project does not allow that executor" },
    risk_screening_pending: { label: "Risk check in progress; only you can answer on the web" },
    basis_required: { label: "Needs your own words or a temporary grant" },
    session_not_direct: { label: "Not a direct chat with you" },
    not_found: { label: "Record not found" },
    conflict: { label: "It changed in the meantime" },
    login_locked: { label: "Too many attempts, try again later" },
    internal: { label: "Server error" },
    models_paused: { label: "Models are paused; resume them in settings" },
  },
  sourceType: { pull: { label: "Scheduled pull" }, script: { label: "Custom script" }, webhook: { label: "Pushed in" } },
  disposition: { pending: { label: "In progress" }, attached: { label: "Added to an item" }, proposed: { label: "Became a proposal" }, ignored: { label: "Skipped" }, unjudged: { label: "Not judged yet" }, expired: { label: "Expired" }, digest: { label: "Brief only" }, alerted: { label: "Alerted directly" } },
  unjudgedReason: {
    model_unavailable: { label: "Waiting for the model", hint: "Judged automatically once it is back" },
    outbound_unset: { label: "Choose what may be sent", hint: "Judged automatically once chosen" },
    undecided: { label: "Waiting for the model", hint: "Judged automatically once it is back" },
    restored: { label: "Put back, waiting for release" },
    models_paused: { label: "Models paused", hint: "Judged automatically once resumed" },
  },
  outboundScope: { full: { label: "Everything" }, excerpt: { label: "Summary and opening" }, meta: { label: "Title only" }, none: { label: "Nothing" } },
  backlogStep: { rules: { label: "Apply rules first" }, score: { label: "Trial scoring" }, bands: { label: "Set the auto-handling lines" }, rejudge: { label: "Release to proposals" }, restore: { label: "Recover skipped messages" } },
  attachState: { pending: { label: "To hand to agent" }, forwarded: { label: "Handed to agent" }, processed: { label: "Processed" }, handed_back: { label: "Handed back to you" }, released: { label: "Back to automatic judgment" } },
  webOnly: {
    funds: { label: "Money", hint: "Involves real payments" },
    external_commitment: { label: "Commitments to others" },
    irreversible: { label: "Irreversible" },
    permission_change: { label: "Permission changes" },
    spec_activation: { label: "Guideline activation" },
  },
  perm: { read: { label: "Read only" }, edit: { label: "Can edit files" }, full: { label: "Fully open", hint: "The host stops asking" } },
  write: { direct: { label: "Takes effect" }, proposed: { label: "Show me first" } },
  muteScope: { source: { label: "Source" }, project: { label: "Project" }, item: { label: "Item" } },
  locality: { local: { label: "My own machine" }, external: { label: "Outside service" } },
  protocol: { "system-one": { label: "Native judgment API" }, "probability-judgment/1": { label: "Self-hosted classifier" } },
  probSource: { native: { label: "Native probability" }, logprobs: { label: "From log-probabilities" }, self_reported: { label: "Self-reported (not usable)" } },
  grantAction: {
    "item.create": { label: "Create items" },
    "item.update": { label: "Edit items" },
    "item.transition": { label: "Change phase or mark done" },
    "item.relate": { label: "Link items" },
    "decision.answer": { label: "Answer decisions" },
    "proposal.accept": { label: "Accept proposals" },
    "proposal.dismiss": { label: "Dismiss proposals" },
    "proposal.attach": { label: "Add proposals to items" },
    "revision.confirm": { label: "Confirm document changes" },
    "revision.reject": { label: "Reject document changes" },
    "delegate.report": { label: "Report for an executor" },
    "followmsg.handback": { label: "Hand back follow-ups" },
  },
  grantStatus: { active: { label: "Active" }, expired: { label: "Expired" }, revoked: { label: "Revoked" } },
  credKind: { observer: { label: "Read only" }, worker: { label: "Worker" }, delegate: { label: "My main assistant" }, ext: { label: "Extension" }, webhook: { label: "Push-in" } },
  executorKind: { "native-subagent": { label: "Host sub-agent" }, "external-cli": { label: "Command-line tool" }, human: { label: "Person" } },
  ruleMode: { off: { label: "Off" }, observe: { label: "Trial", hint: "Records only, does nothing" }, auto: { label: "On" } },
  ruleGroup: { clock_rules: { label: "Scheduled" }, event_rules: { label: "On events" } },
  ruleClass: { reminder: { label: "Remind me" }, action: { label: "Act for me" } },
  judgeUse: {
    inbound_screen: { label: "Is this source message worth a look" },
    capture_prescreen: { label: "Anything to note in this chat" },
    risk_screen: { label: "Must you confirm this yourself" },
    delivery_defer: { label: "Can this decision wait" },
    followup_defer: { label: "Can this reminder wait" },
    verify_gate: { label: "Should you verify this report" },
  },
  judgeTemplate: {
    "triage-screen": { label: "Is this source message worth a look" },
    "capture-prescreen": { label: "Anything to note in this chat" },
    "web-only-screening": { label: "Must you confirm this yourself" },
    "delivery-defer": { label: "Can this decision wait" },
    "follow-up-defer": { label: "Can this reminder wait" },
    "delegation-screen": { label: "Should you verify this report" },
  },
  judgeBackend: { classifier: { label: "Scoring model" }, chat: { label: "Writing model" } },
  decidedBy: { classifier: { label: "Scoring model" }, chat: { label: "Writing model" }, default: { label: "Safe default" } },
  levelOutcome: { decided: { label: "Decided" }, uncertain: { label: "Unsure" }, error: { label: "Error" }, skipped: { label: "Skipped" } },
  spotResult: { should_see: { label: "Should see" }, correct: { label: "Not needed" } },
  healthSignal: {
    source_failures: { label: "Source reads failing" },
    outcall: { label: "Background work queued or timing out" },
    delivery_backlog: { label: "Reminders not going out" },
    chain_trips: { label: "Automation chain stopped" },
    agentsmd_drift: { label: "Repository guidelines out of sync" },
    pack_rejections: { label: "Capability packs failed to load" },
    model_unavailable: { label: "Model unreachable" },
    unjudged_backlog: { label: "Backlog waiting" },
    rule_fallbacks: { label: "Rules set back to trial" },
    receipt_failures: { label: "agent reports failing" },
    other_alerts: { label: "Other alerts" },
    stalled_delegations: { label: "Delegations gone quiet" },
    clock_lag: { label: "Timers running late" },
  },
  doctorCheck: { data_dir: { label: "Data directory" }, config: { label: "Config file" }, tier: { label: "Run tier" }, model_endpoint: { label: "Models" }, sources: { label: "Sources" }, packs: { label: "Capability packs" }, host_plugin: { label: "Host plugin" }, overlaps: { label: "Overlap check" }, channels: { label: "Chat channels" } },
  event: {
    "item.created": { label: "created an item" },
    "item.updated": { label: "edited an item" },
    "item.phase_changed": { label: "changed the phase" },
    "item.deadline_passed": { label: "deadline passed" },
    "item.progress_added": { label: "added progress" },
    "proposal.attached": { label: "added a proposal to an item" },
    "decision.requested": { label: "asked for a decision" },
    "decision.resolved": { label: "made a decision" },
    "delegation.created": { label: "delegated a task" },
    "delegation.reported": { label: "reported a result" },
    "command.rejected": { label: "action rejected" },
    "material.revised": { label: "edited a document" },
    "material.revision_proposed": { label: "proposed a document change" },
    "guideline.activated": { label: "guideline took effect" },
    "project.updated": { label: "edited a project" },
    "project.allowed_executors_changed": { label: "changed the project's executors" },
    "rule.mode_changed": { label: "switched an automatic rule" },
    "allowlist.changed": { label: "changed a list" },
    "policy.changed": { label: "changed settings" },
    "grant.issued": { label: "issued a temporary grant" },
    "grant.revoked": { label: "revoked a temporary grant" },
    "run.started": { label: "background run started" },
    "run.finished": { label: "background run finished" },
    "source.failed": { label: "source read failed" },
    "health.alert": { label: "system alert" },
    "auth.login": { label: "signed in" },
    "auth.login_locked": { label: "sign-in locked" },
    "auth.password_changed": { label: "changed the password" },
    "auth.session_revoked": { label: "signed a session out" },
    "rule.observed": { label: "trial record" },
    "rule.timer_created": { label: "a rule set a timer" },
    "inbound.disposed": { label: "handled a source message" },
    "inbound.requeued": { label: "put back in the backlog" },
    "inbound.captured": { label: "noted from a chat" },
    "instance.created": { label: "created a schedule" },
    "instance.updated": { label: "edited a schedule" },
    "instance.toggled": { label: "turned a schedule on or off" },
    "instance.fired": { label: "schedule fired" },
    "material.revision_rejected": { label: "rejected a document change" },
    "material.revision_summarized": { label: "summarized a change" },
    "followmsg.attached": { label: "follow-up received" },
    "followmsg.processed": { label: "follow-up processed" },
    "followmsg.handed_back": { label: "follow-up handed back" },
    "followmsg.released": { label: "follow-up back to automatic judgment" },
    "executor.declared": { label: "requested access" },
    "executor.confirmed": { label: "approved access" },
    "executor.rejected": { label: "rejected access" },
    "executor.changed": { label: "changed an executor" },
    "executor.removed": { label: "removed an executor" },
    "source.registered": { label: "connected a source" },
    "source.updated": { label: "edited a source" },
    "source.enabled": { label: "enabled a source" },
    "source.disabled": { label: "disabled a source" },
    "item.deleted": { label: "permanently deleted an item" },
    "run.credential_exchanged": { label: "background run picked up its key" },
    "timer.fired": { label: "timer fired" },
  },
  materialKind: { minutes: { label: "Minutes" }, reference: { label: "Reference" }, worknote: { label: "Work notes" }, requirement: { label: "Requirements" }, design: { label: "Design" }, guideline: { label: "Guideline" } },
  pack: {
    brief: { label: "Brief" },
    review: { label: "Review" },
    routine: { label: "Routine" },
    "follow-up": { label: "Follow-up reminders" },
    "decision-card": { label: "Decision cards" },
    "background-run": { label: "Background runs" },
    "guideline-draft": { label: "Guideline drafting" },
    "agents-md": { label: "Repository guideline file" },
    "inbound-triage": { label: "Source message screening" },
    "delegation-verify": { label: "Report verification" },
  },
  rule: {
    "follow-up/follow-up-interval": { label: "Follow up when nothing moves after the due date" },
    "follow-up/stale-check": { label: "Ask me about items quiet for 14 days" },
    "brief/scheduled-brief": { label: "Write briefs on schedule" },
    "review/scheduled-review": { label: "Write reviews on schedule" },
    "routine/scheduled-routine": { label: "Run routines on schedule" },
    "guideline-draft/guideline-check": { label: "Draft a guideline when a project has none" },
    "background-run/handle-labelled": { label: "Start work on items tagged agent-ok" },
    "delegation-verify/verify-report": { label: "Ask me whether to verify executor results" },
  },
  viewField: {
    title: { label: "Title" }, project: { label: "Project" }, created_at: { label: "Created" }, updated_at: { label: "Updated" },
    phase: { label: "Phase" }, summary: { label: "Summary" }, tag: { label: "Tag" }, tags: { label: "Tags" }, deadline: { label: "Deadline" },
    planned_for: { label: "Planned for" }, priority: { label: "Priority" }, "spec.period": { label: "Period" }, "spec.result": { label: "Result" },
  },
  channelRoute: (name) => `Push to ${name}`,
};

export const vocab: Dict<typeof zh> = { zh, en };
