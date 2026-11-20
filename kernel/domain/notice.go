package domain

// AskCode 是内核生成的问题的稳定类别码（Ask.Code，§6.3）：Web 按它和 Ask.Params 显示本地化的一句话，
// Question 只是给 CLI、渠道与旧版界面的原文兜底。取值稳定、只增不改；web/src/app/ids.ts 的 ASK_CODE 与它对照。
// 内核只在"需要本人决定、不定就卡住"时提问（§6.14 第 20 条）；运行故障、被拒的代理写入不是问题，见 RunFaultCode 与命令拒绝事件。
type AskCode string

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
