# 导出 ext

把事项、材料与简报单向写成一个目录下的 Markdown 文件（设计 §10.3）。只读 `/events`、`/items`、`/materials`，游标与状态都在导出目录里。

## 配置

环境变量：`APA_URL`、`APA_EXT_KEY`（导出 ext 只读密钥）、`APA_EXPORT_DIR`（缺省取配置文件的 `root`）、`APA_EXPORT_CONFIG`（YAML，可选）、`APA_PUBLIC_URL`（配置文件没写 `public_url` 时的后备）。不给配置文件时，事项写到 `items/<id>.md`，头部是 v0.1 的那组字段。

```yaml
root: /path/to/export
public_url: https://apa.example      # 动作链接与 Web 地址的主机；没有就不写这两样
select:                              # 缺省：未终结的，加 recent_days（默认 30）天内终结的
  projects: [acc-work]
  phases: [open, waiting]            # 写了 phases 就只按它筛
  kinds: [task]
  tags: [x]
path: "{project}/{kind}/{title}.md"  # {project} {kind} {id} {title} {planned_for} {phase}
front_matter:                        # 导出键: 事项字段（字段名或中文名）；未映射的不写
  title: 标题
  status: 阶段
  plan: 计划日期
date_format: YYYY-MM-DD              # 空则日期原样、时刻为 RFC 3339
body: [summary, body, progress, subitems, materials]   # 节的开关与顺序
action_links: true
briefs: true                         # 简报、复盘与例行任务的结果（kind=brief）各一个文件
```

可映射字段：`id kind title summary status_label priority phase project tags assignee deadline planned_for updated_at version url`。

## 文件与状态

- 标题按文件系统规则清洗；路径被另一个事项占用时，在扩展名前追加 `-<事项 id>`。
- 事项不再满足 `select` 时移到 `root/_archive/` 下；物理删除（读不到）后文件随之删除。
- 启动时按 `GET /items` 对账一次：配置、`select` 变了或停机期间有事项被删，不需要事件也会补上。
- web_only 初筛未返回的事项没有内容可导出，跳过；挂靠消息与来源事件正文从不读取。

## 生成标记与手改检测

头部之后的第一行是标记：

```
<!-- apa-export id=<对象 id> version=<事项版本> hash=<sha256> -->
```

`hash` 是「去掉这一行之后的整个文件内容」的 sha256（十六进制）。下次写入前重算：不一致（或标记缺失）说明被手改过，ext 先把现有文件另存为 `<文件名>.local-edit`，再写新内容，并在 `.apa-export-state.json` 的 `edits` 里记一条。手改不会写回账本。
