package doctor

// 结论的机器码（Check.Code、Detail.Code，设计 §15.5）：Web 按它与 Data 显示本地化说明；Why、Next 是给 CLI 与原始值看的机制性英文。
// 取值稳定，只增不改；web/src/app/ids.ts 的 DOCTOR_CODE 与它对照（ids.check.test.ts）。
// 一个对象有多处问题时，Detail.Code 是第一处，Data["problems"] 按顺序列出全部问题的机器码。
const (
	// 模型端点探测（ProbeJudge、ProbeGenerate；Web 的"测试连接"同用）
	CodeOK             = "ok"
	CodeNotConfigured  = "not_configured"
	CodeNoClient       = "no_client"
	CodeUnreachable    = "unreachable"
	CodeTimeout        = "timeout"
	CodeUnauthorized   = "unauthorized"
	CodeProtocolError  = "protocol_error"
	CodeSelfReported   = "self_reported"
	CodeInvalidRequest = "invalid_request"
	CodeNoAnswer       = "no_answer"
	CodeHTTPStatus     = "http_status"
	CodeSchemaMismatch = "schema_mismatch"
	// CodeDraftSchema：端点能答，但起草建议模板的结构化输出过不了校验（起草每次都失败）；Data["shape"] 是返回内容的形态（有则给）。
	CodeDraftSchema = "draft_schema"
	CodeBadResponse = "bad_response"
	// CodeRegionUnavailable：服务在当前网络位置不可用（HTTP 451 或说明地区受限的 403）。
	CodeRegionUnavailable = "region_unavailable"

	// 检查被跳过（数据目录不可读或结构与程序不符时，其余检查不做）；Data["reason"] 为 CodeLedgerUnreadable 或 CodeLedgerSchemaDiffers
	CodeSkipped             = "skipped"
	CodeLedgerUnreadable    = "ledger_unreadable"
	CodeLedgerSchemaDiffers = "ledger_schema_differs"
	CodeFactsUnreadable     = "facts_unreadable"

	// 数据目录与版本
	CodeDataDirOK          = "data_dir_ok"
	CodeDataDirNoBackup    = "data_dir_no_backup"
	CodeDataDirMissing     = "data_dir_missing"
	CodeDataDirPermissions = "data_dir_permissions"
	CodeLedgerMissing      = "ledger_missing"
	CodeLedgerDamaged      = "ledger_damaged"
	CodeLedgerTooNew       = "ledger_too_new"
	CodeLedgerMigrate      = "ledger_migrate"

	// 运行配置
	CodeConfigOK        = "config_ok"
	CodeConfigInvalid   = "config_invalid"
	CodeConfigExposed   = "config_exposed"
	CodeHostFileInvalid = "host_file_invalid"

	// 运行形态；Data["tier"] 为档位
	CodeTierPaused     = "tier_paused"
	CodeTierKernelOnly = "tier_kernel_only"
	CodeTierNoHost     = "tier_no_host"
	CodeTierFull       = "tier_full"

	// 权限模型（§14.3、§16.5）：authz_switched 已切换到三档模型；authz_not_switched 账本还没切换或策略里仍有旧段，Data["legacy_policy"] 列出旧段
	CodeAuthzSwitched    = "authz_switched"
	CodeAuthzNotSwitched = "authz_not_switched"

	// 模型端点（检查一级）
	CodeModelsPaused  = "models_paused"
	CodeModelsNone    = "models_none"
	CodeModelsOK      = "models_ok"
	CodeModelsFailing = "models_failing"

	// 来源（检查一级）；Data["count"] 为来源数
	CodeSourcesNone            = "sources_none"
	CodeSourcesOK              = "sources_ok"
	CodeSourcesAttention       = "sources_attention"
	CodeSourcesRestoredMissing = "sources_restored_missing"
	// 来源（对象一级）；正常时 Data["last_run"] 为 ok、never、unreadable
	CodeSourceOK                = "source_ok"
	CodeSourceCommandMissing    = "source_command_missing"
	CodeSourceOutboundUnset     = "source_outbound_unset"
	CodeSourceOutboundFull      = "source_outbound_full"
	CodeSourceLastFailed        = "source_last_failed"
	CodeSourceToolTimeout       = "source_tool_timeout"
	CodeSourceToolVersion       = "source_tool_version"
	CodeSourceToolMissing       = "source_tool_missing"
	CodeSourceToolConfig        = "source_tool_config"
	CodeSourceToolFailed        = "source_tool_failed"
	CodeSourceCredsMissing      = "source_creds_missing"
	CodeSourceWebhookKeyMissing = "source_webhook_key_missing"

	// 能力包；Data["loaded"]、Data["rejected"] 为数量
	CodePacksNoLoader = "packs_no_loader"
	CodePacksOK       = "packs_ok"
	CodePacksRejected = "packs_rejected"
	// 能力包（对象一级）；Data["in_effect"] 为生效中的同名包来源（builtin、data），没有则无此键
	CodePackRejected = "pack_rejected"

	// 宿主插件
	CodeHostNone      = "host_none"
	CodeHostOK        = "host_ok"
	CodeHostAttention = "host_attention"
	// 宿主插件（对象一级）
	CodeHostReady            = "host_ready"
	CodeHostReadyNoSelfcheck = "host_ready_no_selfcheck"
	CodeHostHeadlessMissing  = "host_headless_missing"
	CodeHostSelfcheckFailed  = "host_selfcheck_failed"
	CodeHostHookMissing      = "host_hook_missing"

	// 重复提醒
	CodeOverlapsSkipped = "overlaps_skipped"
	CodeOverlapsNone    = "overlaps_none"
	CodeOverlapsFound   = "overlaps_found"
	// 重复提醒（对象一级）；Data["harness"]、Data["kind"]、Data["name"]
	CodeOverlap = "overlap"

	// 渠道与导出
	CodeChannelsNone    = "channels_none"
	CodeChannelsOK      = "channels_ok"
	CodeChannelsBacklog = "channels_backlog"
	// 渠道与导出（对象一级）；Data["last_seen_at"] 没有表示从未来过，Data["backlog"] 为积压数
	CodeChannelOK      = "channel_ok"
	CodeChannelBacklog = "channel_backlog"
	CodeChannelOrphan  = "channel_orphan"
	CodeExportOK       = "export_ok"

	// 凭据保管库（§5.6）；Data["rows"]、["unreadable"]、["other_key"]
	CodeSecretsNone       = "secrets_none"
	CodeSecretsOK         = "secrets_ok"
	CodeSecretsKeyMissing = "secrets_key_missing"
	CodeSecretsKeyMode    = "secrets_key_mode"
	CodeSecretsUnreadable = "secrets_unreadable"

	// 连接（§1.6）
	CodeConnectionsNone    = "connections_none"
	CodeConnectionsOK      = "connections_ok"
	CodeConnectionsProblem = "connections_problem"
	// 连接（对象一级）；Data["name"]、["type"]、["problem"]（问题代码）、["last_ok_at"]、["missing_credentials"]
	CodeConnectionOK       = "connection_ok"
	CodeConnectionProblem  = "connection_problem"
	CodeConnectionDisabled = "connection_disabled"
)
