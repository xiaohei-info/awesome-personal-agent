import { withSession } from "../../app/test-session";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 替身只回答 GET /v1/storage 与 PUT /v1/settings/sources/{id}/content-retention，形状同附录 B.2。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const USAGE = {
  ledger_bytes: 2048, blobs_bytes: 5 * 1024 * 1024, content_bytes: 1536,
  sources: [
    { id: "src_a", name: "邮件", events: 12, content_bytes: 1024, oldest: "2026-01-02T03:04:05Z", retention_days: 90 },
    { id: "src_b", name: "日历", events: 0, content_bytes: 0 },
  ],
};
let keep: Record<string, unknown> = {};
function server(opts: { storage?: Response } = {}) {
  keep = { run_days: 30, ingest_days: 30, judgment_days: 180, last_sweep: { at: "2026-11-23T04:10:00Z", run_dirs: 2, run_dir_bytes: 2048, ingest_bodies: 3, judgments: 4, skipped_in_flight: ["run_x"] } };
  const puts: { url: string; body: unknown }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/storage") return opts.storage ?? json(200, USAGE);
    if (url === "/v1/settings/retention" && init.method === "PUT") {
      const body = JSON.parse(init.body as string);
      puts.push({ url, body });
      if (body.judgment_days === 5) return json(400, { code: "invalid_request", why: "judgment_days must be 0 (never clear) or between 30 and 3650", next: "fix the value and retry" });
      keep = { ...keep, ...body };
      return json(200, keep);
    }
    if (url === "/v1/settings/retention") return json(200, keep);
    if (init.method === "PUT") {
      const body = JSON.parse(init.body as string);
      puts.push({ url, body });
      if (body.days === 99999) return json(400, { code: "invalid_request", why: "days must be an integer from 1 to 36500, or null for no clearing", next: "fix days" });
      return json(200, { days: body.days });
    }
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return { impl, puts };
}
function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/storage"]}>
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}
const row = async (name: string) => within(await screen.findByRole("group", { name }));

beforeEach(() => sessionStorage.clear());

describe("设置 › 存储", () => {
  it("ACT-STO-02: 显示记录文件、附件、来源原文占用，以及各来源事件数、原文大小、最早时间与保留天数（同 GET /storage）", async () => {
    mount(server().impl);
    const totals = within(await screen.findByRole("region", { name: "占用" }));
    expect(totals.getByText("记录文件").nextSibling).toHaveTextContent("2.0 KB");
    expect(totals.getByText("附件").nextSibling).toHaveTextContent("5.0 MB");
    expect(totals.getByText("来源原文").nextSibling).toHaveTextContent("1.5 KB");
    const a = await row("邮件");
    expect(a.getByText("事件数: 12")).toBeInTheDocument();
    expect(a.getByText("原文大小: 1.0 KB")).toBeInTheDocument();
    expect(a.getByText(/最早一条: .*2026/)).toBeInTheDocument();
    expect(a.getByLabelText("邮件 保留天数")).toHaveValue(90);
    const b = await row("日历");
    expect(b.getByText("最早一条: —")).toBeInTheDocument();
    expect(b.getByLabelText("日历 保留天数")).toHaveValue(null); // 默认不清理
    expect(screen.getByText("超期的原文会被清掉，无法恢复。")).toBeInTheDocument();
  });

  it("设保留天数发 PUT {days:n}；清空发 {days:null}（不清理）", async () => {
    const s = server();
    mount(s.impl);
    const b = await row("日历");
    await userEvent.type(b.getByLabelText("日历 保留天数"), "30");
    await userEvent.click(b.getByRole("button", { name: "保存日历保留天数" }));
    await b.findByRole("status");
    const a = await row("邮件");
    await userEvent.clear(a.getByLabelText("邮件 保留天数"));
    await userEvent.click(a.getByRole("button", { name: "保存邮件保留天数" }));
    await a.findByRole("status");
    expect(s.puts).toEqual([
      { url: "/v1/settings/sources/src_b/content-retention", body: { days: 30 } },
      { url: "/v1/settings/sources/src_a/content-retention", body: { days: null } },
    ]);
  });

  it("本地拒绝非整数不发请求；服务端拒绝时原因原样显示、输入保留", async () => {
    const s = server();
    mount(s.impl);
    const b = await row("日历");
    const input = b.getByLabelText("日历 保留天数");
    await userEvent.type(input, "1.5");
    await userEvent.click(b.getByRole("button", { name: "保存日历保留天数" }));
    expect(await b.findByRole("alert")).toHaveTextContent("保留天数须是 1 到 36500 的整数");
    expect(s.puts).toHaveLength(0);
    await userEvent.clear(input);
    await userEvent.type(input, "99999");
    await userEvent.click(b.getByRole("button", { name: "保存日历保留天数" }));
    expect(await b.findByRole("alert")).toHaveTextContent(/36500/);
    expect(input).toHaveValue(99999);
  });

  it("读取失败显示原因并可重试，不显示假数据", async () => {
    mount(server({ storage: json(500, { code: "internal", why: "boom", next: "retry" }) }).impl);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "占用" })).toBeNull();
  });

  it("三项保留期可改：显示上次清理报数；改成 7 天发 PUT；被拒绝时原因原样显示、输入保留；文案不出现表名", async () => {
    const s = server();
    mount(s.impl);
    const keepPanel = within(await screen.findByRole("region", { name: "运行数据保留" }));
    expect(await keepPanel.findByLabelText("last-sweep")).toHaveTextContent("删除运行目录 2 个（2.0 KB）、请求体 3 条、判断记录 4 条，在途运行 1 个没动");
    expect(keepPanel.getByText(/保留：运行记录与结论/)).toBeInTheDocument();
    expect(keepPanel.queryByText(/outcall|webhook_ingest|outcall_judgment/)).toBeNull();
    const run = keepPanel.getByLabelText("运行目录（天）");
    expect(run).toHaveValue(30);
    await userEvent.clear(run);
    await userEvent.type(run, "7");
    await userEvent.click(keepPanel.getByRole("button", { name: "保存保留期" }));
    await keepPanel.findByText("保留期已保存，下次清理起按新值");
    expect(s.puts.at(-1)).toEqual({ url: "/v1/settings/retention", body: { run_days: 7, ingest_days: 30, judgment_days: 180 } });
    const j = keepPanel.getByLabelText("判断记录（天）");
    await userEvent.clear(j);
    await userEvent.type(j, "5");
    await userEvent.click(keepPanel.getByRole("button", { name: "保存保留期" }));
    expect(await keepPanel.findByRole("alert")).toHaveTextContent("judgment_days must be 0 (never clear) or between 30 and 3650");
    expect(j).toHaveValue(5);
  });
});
