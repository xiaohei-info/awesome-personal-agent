// 内核取值的前端镜像（设计 §4）：界面按这些 ID 查 vocab.ts 的显示名，ID 本身只在「显示原始值」里可见。
// 每组注明出处；ids.check.test.ts 读这些出处，内核里有而这里没有时失败。
// 这里可以比内核多（例如先放进另一个改动即将新增的取值），不能比内核少。

/** 事项阶段：kernel/domain/phase.go */
export const PHASE = ["proposed", "open", "waiting", "needs_input", "done", "dropped"] as const;
/** 优先级：kernel/domain/item.go Priority* */
export const PRIORITY = ["low", "normal", "high", "urgent"] as const;
/** 事项类型：kernel/domain/item.go Kind*；routine 来自 packs/routine */
export const ITEM_KIND = ["task", "commitment", "decision", "delegation", "brief", "routine"] as const;
/** 等待对象：kernel/domain/item.go Waiting* */
export const WAITING = ["human", "executor", "external", "time"] as const;
/** 事项关系：kernel/domain/item.go Rel* */
export const RELATION = ["relates", "parent", "blocks", "derived_from", "supersedes", "basis", "output", "minutes"] as const;
/** 项目状态：kernel/domain/entities.go ProjectStatus */
export const PROJECT_STATUS = ["active", "paused", "archived"] as const;
/** 投递类别：kernel/domain/entities.go DeliveryClass */
export const DELIVERY_CLASS = ["decision", "brief", "reminder", "health", "wake"] as const;
/** 投递状态：kernel/domain/entities.go DeliveryState */
export const DELIVERY_STATE = ["pending", "claimed", "delivered", "acknowledged", "fallback_web", "superseded"] as const;
/** 路由：kernel/delivery/delivery.go Route*；另有 channel:<渠道名>，见 vocab 的 channelRoute */
export const ROUTE = ["inject", "wake", "web"] as const;
/** 被压成只留未读的原因：kernel/delivery/disturb.go Suppressed* */
export const SUPPRESSED = ["daily_cap", "muted"] as const;
/** 运行类别：kernel/domain/entities.go RunCategory */
export const RUN_CATEGORY = ["judge", "generate", "task", "background", "source", "transform", "reader"] as const;
/** 运行状态：kernel/domain/entities.go RunState */
export const RUN_STATE = ["queued", "running", "succeeded", "failed", "timeout"] as const;
/** 运行者（§13.6，内核按字面量比较，无常量） */
export const RUNNER = ["task", "main_session"] as const;
/** 拉起方式（§13.6；kernel/runidentity LaunchWake） */
export const LAUNCH = ["headless", "wake"] as const;
/** 回执结果：kernel/delegation/delegation.go Outcome* */
export const OUTCOME = ["progress", "done", "blocked"] as const;
/** 操作者类型与入口：kernel/domain/event.go ActorType、Via */
export const ACTOR_TYPE = ["human", "agent", "system"] as const;
export const VIA = ["web", "cli", "clock", "rule", "source", "ext"] as const;
/** 会话类型：kernel/domain/event.go Session* */
export const SESSION_KIND = ["direct", "group", "unknown"] as const;
/** 错误码：kernel/domain/errors.go Code。models_paused 由「暂停模型」改动新增 */
export const ERROR_CODE = [
  "invalid_request", "unauthenticated", "permission_denied", "web_only", "executor_not_allowed", "risk_screening_pending",
  "basis_required", "session_not_direct", "not_found", "conflict", "login_locked", "internal", "models_paused",
] as const;
/** 错误码里只说了类别、要靠服务端 why 才说得清的：显示「类别：why」 */
export const GENERIC_ERROR: readonly string[] = ["invalid_request", "conflict", "internal"];

/** 来源类型：kernel/domain/entities.go SourceType */
export const SOURCE_TYPE = ["pull", "script", "webhook"] as const;
/** 来源事件处置：kernel/domain/entities.go Disposition */
export const DISPOSITION = ["pending", "attached", "proposed", "ignored", "unjudged", "expired", "digest", "alerted"] as const;
/** 未判定原因：kernel/domain/entities.go Unjudged*。models_paused 由「暂停模型」改动新增 */
export const UNJUDGED_REASON = ["model_unavailable", "outbound_unset", "undecided", "restored", "models_paused"] as const;
/** 外发范围：kernel/sources/registry/outbound.go Scope* */
export const OUTBOUND_SCOPE = ["full", "excerpt", "meta", "none"] as const;
/** 积压放开的步骤（§9.3；Web 自己的流程，不是内核常量） */
export const BACKLOG_STEP = ["rules", "score", "bands", "rejudge", "restore"] as const;
/** 挂靠消息状态：kernel/sources/runner/runner.go Attach* */
export const ATTACH_STATE = ["pending", "forwarded", "processed", "handed_back", "released"] as const;

/** web_only 默认类别：kernel/policy/policy.go Category* */
export const WEB_ONLY = ["funds", "external_commitment", "irreversible", "permission_change", "spec_activation"] as const;
/** 运行权限档：kernel/policy/policy.go Perm* */
export const PERM = ["read", "edit", "full"] as const;
/** 材料写入策略：kernel/policy/policy.go Write* */
export const WRITE = ["direct", "proposed"] as const;
/** 静音范围：kernel/policy/policy.go Mute* */
export const MUTE_SCOPE = ["source", "project", "item"] as const;
/** 端点位置：kernel/policy/policy.go Locality* */
export const LOCALITY = ["local", "external"] as const;
/** 判断协议：kernel/policy/policy.go Protocol*；对数概率换算的协议在 SPIKE-LOGPROB（§21.3）有结论前 Web 不提供，前端产物里不得出现它的名字（ACT-JDP-11）。 */
export const PROTOCOL = ["system-one", "probability-judgment/1"] as const;
/** 概率来源（§8.8；内核按字面量写入） */
export const PROB_SOURCE = ["native", "logprobs", "self_reported"] as const;
/** 可授权的动作：kernel/policy/grant.go GrantableActions */
export const GRANT_ACTION = [
  "item.create", "item.update", "item.transition", "item.relate", "decision.answer", "proposal.accept", "proposal.dismiss",
  "proposal.attach", "revision.confirm", "revision.reject", "delegate.report", "followmsg.handback",
] as const;
/** 授权单状态（Web 由 revoked_at、expires_at 推出） */
export const GRANT_STATUS = ["active", "expired", "revoked"] as const;

/** 凭据种类：kernel/auth/auth.go Kind */
export const CRED_KIND = ["observer", "worker", "delegate", "ext", "webhook"] as const;
/** 执行者种类：kernel/executors/executors.go Kind* */
export const EXECUTOR_KIND = ["native-subagent", "external-cli", "human"] as const;

/** 规则模式：kernel/rules/rules.go Mode* */
export const RULE_MODE = ["off", "observe", "auto"] as const;
/** 规则组：kernel/packs/pack.go 的 yaml 键 */
export const RULE_GROUP = ["clock_rules", "event_rules"] as const;
/** 规则类别（§14.8，由加载器推定） */
export const RULE_CLASS = ["reminder", "action"] as const;

/** 判断用途与内置模板：kernel/agents/direct/judge.go builtinUse（模板 → 用途） */
export const JUDGE_USE = ["inbound_screen", "capture_prescreen", "risk_screen", "delivery_defer", "followup_defer", "verify_gate"] as const;
export const JUDGE_TEMPLATE = ["triage-screen", "capture-prescreen", "web-only-screening", "delivery-defer", "follow-up-defer", "delegation-screen"] as const;
/** 判断后端、定案者、逐级结果：kernel/judgment/record/record.go */
export const JUDGE_BACKEND = ["classifier", "chat"] as const;
export const DECIDED_BY = ["classifier", "chat", "default"] as const;
export const LEVEL_OUTCOME = ["decided", "uncertain", "error", "skipped"] as const;
/** 抽检结论：kernel/judgment/spotcheck/spotcheck.go */
export const SPOT_RESULT = ["should_see", "correct"] as const;

/** 健康信号：api/health/health.go 的 sig(...) */
export const HEALTH_SIGNAL = [
  "source_failures", "outcall", "delivery_backlog", "chain_trips", "agentsmd_drift", "pack_rejections", "model_unavailable",
  "unjudged_backlog", "rule_fallbacks", "receipt_failures", "other_alerts", "stalled_delegations", "clock_lag",
] as const;
/** 自检项：kernel/doctor/report.go Order */
export const DOCTOR_CHECK = ["data_dir", "config", "tier", "model_endpoint", "sources", "packs", "host_plugin", "overlaps", "channels"] as const;

/** 对外事件类型：kernel/domain/event.go Ev*（附录 B.4） */
export const EVENT = [
  "item.created", "item.updated", "item.phase_changed", "item.deadline_passed", "item.progress_added", "proposal.attached",
  "decision.requested", "decision.resolved", "delegation.created", "delegation.reported", "command.rejected",
  "material.revised", "material.revision_proposed", "guideline.activated", "project.updated", "project.allowed_executors_changed",
  "rule.mode_changed", "allowlist.changed", "policy.changed", "grant.issued", "grant.revoked", "run.started", "run.finished",
  "source.failed", "health.alert", "auth.login", "auth.login_locked", "auth.password_changed", "auth.session_revoked",
] as const;
/** 内部审计事件类型（各模块写入，不在对外清单里；查不到的显示原值） */
export const EVENT_INTERNAL = [
  "rule.observed", "rule.timer_created", "inbound.disposed", "inbound.requeued", "inbound.captured", "instance.created",
  "instance.updated", "instance.toggled", "instance.fired", "material.revision_rejected", "material.revision_summarized",
  "followmsg.attached", "followmsg.processed", "followmsg.handed_back", "followmsg.released", "executor.declared",
  "executor.confirmed", "executor.rejected", "executor.changed", "executor.removed", "source.registered", "source.updated",
  "source.enabled", "source.disabled", "item.deleted", "run.credential_exchanged", "timer.fired",
] as const;

// 以下是开放集合：能力包可以扩展，词表只登记内置的，查不到时显示原值。
/** 材料 kind（内置写入策略见 kernel/policy/policy.go Default） */
export const MATERIAL_KIND = ["minutes", "reference", "worknote", "requirement", "design", "guideline"] as const;
/** 内置能力包：packs/ 下各目录 */
export const PACK = ["brief", "review", "routine", "follow-up", "decision-card", "background-run", "guideline-draft", "agents-md", "inbound-triage", "delegation-verify"] as const;
/** 内置规则，按 包名/规则 id：packs/<包名>/pack.yaml */
export const BUILTIN_RULE = [
  "follow-up/follow-up-interval", "follow-up/stale-check", "brief/scheduled-brief", "review/scheduled-review",
  "routine/scheduled-routine", "guideline-draft/guideline-check", "background-run/handle-labelled", "delegation-verify/verify-report",
] as const;
/** 视图字段（附录 C.8，packs/<包名>/views） */
export const VIEW_FIELD = ["title", "project", "created_at", "updated_at", "phase", "summary", "tag", "tags", "deadline", "planned_for", "priority", "spec.period", "spec.result"] as const;
