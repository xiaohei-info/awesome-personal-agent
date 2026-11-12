# 合成账本夹具（v0.1 结构账本）

按 `.omac/apa-v02.design/OMAC-NOTES.md` §5.1 生成的**合成**账本，用于升级/回退演练与积压放开的替身验收。不含任何真实数据（地址是 `example.invalid`，端点是丢弃端口，秘密是固定的合成串）。

```bash
# 每个已冻结结构版本一份，打印 export LEDGER_FIXTURE_V<n>=…
go run ./tests/fixtures/ledger/cmd/gen -out "$(mktemp -d)"
# 指定任一结构版本 2..主干最新（同内容，更新的表/列取默认值）
go run ./tests/fixtures/ledger/cmd/gen -version 3 -out /tmp/fx3
# 旧版本主程序与宿主插件包（APA_V01_BIN / APA_V01_PLUGIN）
eval "$(tests/fixtures/ledger/fetch-v01.sh /tmp/apa-v01)"
```

夹具目录就是一个完整的 `APA_HOME`（`ledger.db`、`config.yaml`、`packs/`），`cp -R` 得到一次性副本；根目录的 `manifest.json` 给出全部期望基线（键名同验收文档 ACT-ENV-08）。生成是确定的：同版本两次生成逐字节相同。

- **冻结版本**登记在 `fixture.go` 的 `Frozen`。版本 3 在第一次有真实账本升级到它之后才冻结，到时往 `Frozen` 加一行（含冻结提交）。
- **`APA_V01_BIN` 不是 v0.1.1 标签**：标签早于 `sources_runner` 的第 2 步迁移（未判定原因，#254），而账本结构版本 2 包含它；v0.1.1 的二进制会把结构版本 2 的账本当成「比自己新」。`fetch-v01.sh` 缺省从 `Frozen[2]`（结构版本 2 的最后一次变更）构建。
- `config.yaml` 带 `unjudged_rejudge_days: 0`：夹具的时间固定在过去，不冻结的话任何一次启动都会按真实时钟把积压转成 `expired`。
- 夹具只用结构版本 2 就有的列写入；`Generate` 在内核当前迁移上建库，所以结构演进后夹具自动随之派生。
- 迁移失败注入（`APA_TEST_MIGRATION_FAIL_AFTER`）只在 `-tags apa_testclock` 的测试构建里生效：`go test -tags apa_testclock ./tests/fixtures/...`。
- **冻结证据**：清单的 `migration_checksums` 列出冻结版本每一步迁移的校验和，`TestManifestMigrationChecksumsMatchSource` 从当前源码重算并与冻结时的摘要比对。`migration_files` 只用于定位源文件：这些文件会随后续版本的迁移追加而变，所以验收文档 ACT-UPG-19 的整文件 `diff` 对它们不成立，应改为核对 `migration_checksums`（需走验收文档修订）。
