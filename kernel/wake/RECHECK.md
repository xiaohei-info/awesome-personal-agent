# 唤醒租约复验记录（缺陷 dfx-5410a4c832ad，GT-296）

v0.2.0 不改唤醒与租约。本记录用替身（STUB_HARNESS、插件契约测试、内核/HTTP 单元测试）逐条复验
`flow-wake-lease` 的 ACT-WAK-02..09，全部通过，未发现需要修复的缺陷，无代码改动。

| Action | 复验测试 |
|---|---|
| ACT-WAK-02 唤醒投递，内核不外呼 | `kernel/wake` TestOpenProducesWakeDeliveryOnly；`kernel/background` TestWakeDeclarationLaunchesWakeRun；`kernel/delivery` TestWakeDeliveryCarriesRunID |
| ACT-WAK-03 同一把 delegate token 领取并换凭据 | `api/runs` TestExchangeWakeRunCredentialRoute；`kernel/runidentity` TestExchangeForWakeRun；`plugins/hermes/tests/wake/test_wake.py::test_wake_delivery_opens_turn_with_run_id_and_no_credential` |
| ACT-WAK-04 租约内不能代答，授权单也不行 | `kernel/wake` TestLeaseBlocksDelegateAnswerUntilReceipt |
| ACT-WAK-05 运行凭据不能代答、限目标事项 | `kernel/runidentity` TestRunCredentialCannotDecide、TestRunCredentialIsScopedToTarget、TestRunCredentialActorCarriesRunID；`cli/serve` TestRunCredentialReadsOnlyTargetItemJudgmentRecords |
| ACT-WAK-06 回执后租约释放、旧凭据失效 | `kernel/wake` TestLeaseBlocksDelegateAnswerUntilReceipt、TestAttachSettlesOnCommit |
| ACT-WAK-07 超时释放租约 | `kernel/wake` TestLeaseEndsWithRunOrTimeoutAndIsPerToken；`kernel/runidentity` TestRunCredentialExpiresWithTheRun |
| ACT-WAK-08 worker 换取不开租约，403/409/再换取 | `kernel/runidentity` TestExchangeForWakeRun；`kernel/wake` TestLeaseEndsWithRunOrTimeoutAndIsPerToken；`api/runs` TestExchangeWakeRunCredentialRoute |
| ACT-WAK-09 唤醒回合需要本人时转 needs_input | `kernel/background` TestStillOpenAfterRunGoesToNeedsInput、TestRunnerQuestionIsKept |

转本机验证 L9：真实网关里注入回合、插件工具可调用，以及真实环境的租约行为；替身无法复现。
