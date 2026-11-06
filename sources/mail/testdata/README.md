# 录制样本

每个子目录是一个声明版本（`Supported[].Sample`）的工具输出样本，契约测试据此回放，CI 不连真实账号。

## 1.1.0

在专用测试邮箱上用 himalaya v1.1.0 实录，再脱敏。`-c` 指向临时目录里的配置文件，不使用用户自己的 himalaya 配置。

```sh
himalaya -c "$CFG" --version | head -1                                  # version.txt
himalaya -c "$CFG" envelope list --output json --page-size 50           # list.json（只取 3 封测试邮件）
himalaya -c "$CFG" message read --preview -H Message-ID --output json N # read-N.json
```

脱敏：地址换成 `agent@example.com`，Message-ID 换成 `<mid-N@example.com>`，正文换成占位 `BODY-PLACEHOLDER`；其余字段（字段名、flags、日期格式、签名行）保持实录原样。

实测结论：`envelope list` 的 JSON 只有 `id`、`flags`、`subject`、`from`、`to`、`date`、`has_attachment`，没有 `message_id`；Message-ID 经 `message read --preview -H Message-ID` 取得，输出是一个 JSON 字符串 `"Message-ID: <…>\n\n正文"`。

## 2.0.0

未在真实账号上实测，是按设计假设手写的样本（envelope JSON 带 `message_id`）。拿到 2.x 的真实输出后，按 1.1.0 的方法重录并核对 `Supported` 里的取法。
