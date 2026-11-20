package domain

// AskCode 是内核生成的问题的稳定类别码（Ask.Code，§6.3）：Web 按它和 Ask.Params 显示本地化的一句话，
// Question 只是给 CLI、渠道与旧版界面的原文兜底。取值稳定、只增不改；web/src/app/ids.ts 的 ASK_CODE 与它对照。
// 内核只在"需要本人决定、不定就卡住"时提问（§6.14 第 20 条）；运行故障、被拒的代理写入不是问题，见 RunFaultCode 与命令拒绝事件。
type AskCode string

// HandleOrDrop 是"内核问本人怎么收尾"类问题的两个真实选项：我来处理（回到 open）、放弃（转 dropped）。
// 不给"重新运行""改派"：回答不会再次拉起运行，也不会换执行者，给了就是假选项（§6.14 第 20 条）。
func HandleOrDrop() []AskOption {
	return []AskOption{{ID: "handle", Label: "我来处理"}, {ID: "drop", Label: "放弃", Next: PhaseDropped}}
}

const (
	AskTaskIncomplete       AskCode = "task_incomplete"       // 任务型运行跑完了，结果没有写回（没落账）；params.detail
	AskTaskRefused          AskCode = "task_refused"          // 任务型运行没有拉起（配置冲突等）；params.detail
	AskBackgroundIncomplete AskCode = "background_incomplete" // 后台运行跑完了，事项仍是 open、没有交回；params.detail
	AskBackgroundRefused    AskCode = "background_refused"    // 后台运行没有拉起（档位、检出、执行者等）；params.detail
	AskResumeRefused        AskCode = "resume_refused"        // 续接复核没通过；params.detail
	AskRuleActionFailed     AskCode = "rule_action_failed"    // 实例规则建的事项后续动作失败；params.detail
	AskSourceAuthFailed     AskCode = "source_auth_failed"    // 来源认证或配置失败；params.source
	AskGuidelineNoExecutor  AskCode = "guideline_no_executor" // 没有起草项目规范的执行者
	AskAgentsMDDrift        AskCode = "agentsmd_drift"        // 仓库的 AGENTS.md 被手改；params.repo
	AskHostedMissing        AskCode = "hosted_missing"        // 托管文档路径已不存在；params.path、params.repo
	AskVerifyDelegation     AskCode = "verify_delegation"     // 委派回执完成，需要本人核实；params.title、params.low_confidence
	AskFollowupRoundsUsed   AskCode = "followup_rounds_used"  // 跟进轮次用完；params.title、params.rounds
)

// RunFaultCode 是一次运行失败的稳定类别码（Run.Fault，§6.11）：由外呼调用器按终态、退出码与失败附注归类，
// Web 按它显示"出了什么事、下一步做什么"。它是事件，不是问题：运行故障不把事项转 needs_input。
type RunFaultCode string

const (
	FaultStartFailed  RunFaultCode = "start_failed"   // 没能拉起进程（工作目录、运行上下文、Agent 配置不可用等）
	FaultExecDenied   RunFaultCode = "exec_denied"    // 退出码 126：执行程序找到了但没有权限运行
	FaultExecNotFound RunFaultCode = "exec_not_found" // 退出码 127：执行程序找不到
	FaultExitNonzero  RunFaultCode = "exit_nonzero"   // 程序运行后以非 0 退出；params.exit
	FaultKilled       RunFaultCode = "killed"         // 进程被信号终止（不是超时）
	FaultTimeout      RunFaultCode = "timeout"        // 超时被终止；params.timeout
	FaultLost         RunFaultCode = "lost"           // 内核关停或重启时进程不在了
)

// Fault 是运行失败的类别码与参数；参数只含机制值（退出码、时限），不含输出与环境。
type Fault struct {
	Code   RunFaultCode      `json:"code"`
	Params map[string]string `json:"params,omitempty"`
}

// NoticeCode 是"动作没能完成"的稳定类别码：规则动作失败、运行在拉起前被拒（§6.14 第 20 条）。它们是事件不是问题：
// 记在 health.alert 载荷的 code 与 params 里，事项不转 needs_input；规则自己建的事项转 dropped（§8.7）。
// Web 按它说人话并给下一步；web/src/app/ids.ts 的 NOTICE_CODE 与它对照。取值稳定、只增不改。
type NoticeCode string

const (
	NoticeTaskRefused       NoticeCode = "task_refused"       // 任务型没有拉起（缺任务说明、权限档位、没有检出、挂靠消息不在转发白名单）；params.detail
	NoticeBackgroundRefused NoticeCode = "background_refused" // 后台运行没有拉起（规则模式、档位、执行者名单、检出等）；params.detail
	NoticeResumeRefused     NoticeCode = "resume_refused"     // 本人回答后续接复核没通过；params.detail
	NoticeModelUnset        NoticeCode = "model_unset"        // 动作需要的模型端点没有配置
	NoticeModelsPaused      NoticeCode = "models_paused"      // 本人暂停了模型
	NoticeOutboundBlocked   NoticeCode = "outbound_blocked"   // 外发范围不允许把内容送往模型端点
	NoticeChainTripped      NoticeCode = "chain_tripped"      // 链路深度超过阈值，没有派发
	NoticeActionRejected    NoticeCode = "action_rejected"    // 动作被授权规则拦下；params.code 是拒绝原因码（domain.Code）
	NoticeActionFailed      NoticeCode = "action_failed"      // 其余失败；原因只在操作记录里
)

// NotStarted 是带类别码的"动作没能完成"：动作处理器返回它，规则引擎据此记健康告警（§8.7）。
// Detail 是内核生成的机制性原因，不含账本内容；Item 是受影响的事项（可空）；Cause 是底层错误（可空，errors.Is 可达）。
type NotStarted struct {
	Code   NoticeCode
	Detail string
	Item   string
	Cause  error
}

func (e *NotStarted) Error() string { return string(e.Code) + ": " + e.Detail }
func (e *NotStarted) Unwrap() error { return e.Cause }
