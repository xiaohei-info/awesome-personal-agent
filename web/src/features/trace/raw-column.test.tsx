import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as sources } from "../sources";
import type { TraceData } from "./Trace";

// 测试边界替身：按 GET /v1/source-events/{id}（原文，附录 B.2/B.6）与 /trace 的契约应答；形状取自 api/sources/rawread.go 的 eventRead。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const KERNEL = { type: "system", principal: "kernel:inbound", via: "source" };
const AT = "2030-01-01T09:20:00Z";

const BODY = "第一行\n    缩进的第二行\n\n第四行 <b>不是标签</b>";
const HTML = '<html><img src="http://127.0.0.1:9/track.png"><script>window.__ran = 1</script><p>hi</p></html>';
const ev = (id: string) => ({ id, source: "src_mail", type: "mail.received", subject: "季度报表", sender: "bob@example.com", sender_verified: true, disposition: "proposed", occurred_at: "2030-01-01T09:00:00Z" });
const MAIL = {
  event: ev("sev_mail"),
  content: { state: "full", bytes: 2048, parts: [{ part: "body", media_type: "text/plain", bytes: 100, sha256: "x" }] },
  body: BODY,
  original: { media_type: "text/html", text: HTML },
  headers: { From: "bob@example.com", Received: ["hop1", "hop2"] },
  attachments: [{ name: "报表.xlsx", media_type: "application/vnd.ms-excel", size: 3 * 1024 * 1024 }],
  scope: "full",
};
const CHAT = {
  event: { ...ev("sev_chat"), source: "capture:prj_a", type: "chat.captured" },
  content: { state: "full", bytes: 300, parts: [{ part: "messages", media_type: "application/json", bytes: 300, sha256: "y" }] },
  messages: [
    { sender: "owner", text: "先改登录页", at: "2030-01-01T08:00:00Z" },
    { sender: "agent", text: "好的，\n再查一下超时", at: "2030-01-01T08:01:00Z" },
  ],
  scope: "full",
};
const state = (id: string, content: object, extra: object = {}) => ({ event: ev(id), content: { bytes: 10, parts: [], ...content }, scope: "full", ...extra });
const READS: Record<string, unknown> = {
  sev_mail: MAIL,
  sev_chat: CHAT,
  sev_partial: state("sev_partial", { state: "partial", bytes: 500, missing: [{ part: "body", reason: "mail server timed out" }] }, { body: "只有开头" }),
  sev_legacy: state("sev_legacy", { state: "legacy", bytes: 80 }, { body: "旧事件正文" }),
  sev_purged: state("sev_purged", { state: "purged", bytes: 0 }),
  sev_old: state("sev_old", { state: "legacy", bytes: 80 }, { body: "旧" }),
};
const step = (type: string, payload: unknown) => ({ seq: 1, type, object: "x", actor: KERNEL, at: AT, payload });
const trace = (id: string, entries: TraceData["entries"]): TraceData => ({ message: ev(id), entries });
const arrived = { at: AT, who: { kind: "source", name: "src_mail" }, kind: "arrived", args: {}, steps: [] };
const judged = (material?: unknown) => ({
  at: AT, who: { kind: "model", name: "jev-1", role: "judge" }, kind: "judged", steps: [],
  args: { levels: [{ backend: "classifier", model: "jev-1", p: 0.7, seg: "above", outcome: "decided", ...(material ? { material } : {}) }], low: 0.15, high: 0.6, decided_by: "classifier", answer: "yes" },
});
const proposed = (payload: unknown) => ({ at: AT, who: { kind: "system" }, kind: "proposed", item: { id: "itm_1", title: "回复报表" }, args: {}, steps: [step("inbound.disposed", payload)] });
const TRACES: Record<string, TraceData> = {
  sev_mail: trace("sev_mail", [
    arrived,
    judged({ sent_chars: 8000, original_chars: 20000, mode: "head-tail" }),
    proposed({ disposition: "proposed", material: { sent_chars: 24000, original_chars: 61000, mode: "head-tail" } }),
  ]),
  // 升级前已处置：判断记录与处置载荷都没有 material。
  sev_old: trace("sev_old", [arrived, judged(), proposed({ disposition: "proposed" })]),
};

let fail: string | null = null;
let reads: string[] = [];
const backend = () =>
  (async (url: string) => {
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_mail", name: "gmail 邮件" }] });
    const t = /^\/v1\/source-events\/([^/]+)\/trace$/.exec(url);
    if (t) return json(200, TRACES[t[1]] ?? trace(t[1], [arrived]));
    const r = /^\/v1\/source-events\/([^/]+)$/.exec(url);
    if (r) {
      reads.push(url);
      if (fail === r[1]) return json(500, { code: "internal", why: "ledger read failed", next: "retry" });
      return READS[r[1]] ? json(200, READS[r[1]]) : json(404, { code: "not_found", why: "no such event", next: "" });
    }
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;

function mount(at: string) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[sources]} fetchImpl={backend()} />
    </MemoryRouter>,
  );
}
const column = async () => within(await screen.findByRole("region", { name: "原文" }));

beforeEach(() => {
  sessionStorage.clear();
  reads = [];
  fail = null;
  (window as unknown as { __ran?: number }).__ran = undefined;
});

describe("处理经过页的原文栏（ACT-WRP-01）", () => {
  it("邮件：正文纯文本保留换行与缩进，头部成表，附件列出名字类型大小，页头写大小与完整", async () => {
    mount("/sources/events/sev_mail");
    const col = await column();
    const body = await col.findByText(/第一行/);
    expect(body.textContent).toBe(BODY); // 逐字：换行、缩进、尖括号都在
    expect(body.tagName).toBe("PRE");
    expect(body.querySelector("b")).toBeNull(); // 没当成标签渲染
    const head = col.getByRole("table", { name: "头部" });
    expect(within(head).getByRole("row", { name: /From\s*bob@example.com/ })).toBeInTheDocument();
    expect(within(head).getByRole("row", { name: /Received\s*hop1\s*hop2/ })).toBeInTheDocument();
    const att = col.getByRole("table", { name: "附件" });
    expect(within(att).getByRole("row", { name: /报表\.xlsx\s*application\/vnd\.ms-excel\s*3\.0 MiB/ })).toBeInTheDocument();
    expect(col.getByText("原文 2.0 KiB · 完整")).toBeInTheDocument();
    expect(reads).toEqual(["/v1/source-events/sev_mail"]);
  });

  it("会话事件：逐条列出发送者、时刻与文字", async () => {
    mount("/sources/events/sev_chat");
    const col = await column();
    const list = await col.findByRole("list", { name: "会话消息" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("owner");
    expect(rows[0].textContent).toContain("先改登录页");
    expect(rows[1].textContent).toContain("agent");
    expect(within(rows[1]).getByText(/好的，\s*再查一下超时/).tagName).toBe("PRE");
    expect(rows[0].querySelector("time")).toHaveAttribute("datetime", "2030-01-01T08:00:00Z");
    expect(col.queryByRole("table", { name: "附件" })).toBeNull();
  });

  it("读取失败：栏内如实报错，处理经过照常显示", async () => {
    fail = "sev_mail";
    mount("/sources/events/sev_mail");
    const col = await column();
    expect((await col.findAllByText(/ledger read failed/)).length).toBeGreaterThan(0);
    expect(await screen.findByRole("list", { name: "处理经过" })).toBeInTheDocument();
  });
});

describe("原始格式只作文本（ACT-WRP-02）", () => {
  it("切到原始格式：HTML 源码原样显示为文本，不渲染、不执行、不产生外部资源节点", async () => {
    mount("/sources/events/sev_mail");
    const col = await column();
    expect(await col.findByText(/第一行/)).toBeInTheDocument();
    expect(col.queryByText(HTML)).toBeNull(); // 默认不显示原始格式
    const toggle = col.getByRole("button", { name: "原始格式" });
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-pressed", "true");
    const raw = col.getByText(HTML);
    expect(raw.tagName).toBe("PRE");
    expect(raw.children).toHaveLength(0);
    expect(document.querySelector("img[src*='127.0.0.1'], script, iframe, object, embed")).toBeNull();
    expect((window as unknown as { __ran?: number }).__ran).toBeUndefined();
    expect(col.getByText("text/html")).toBeInTheDocument();
    await userEvent.click(col.getByRole("button", { name: "纯文本" }));
    expect(col.queryByText(HTML)).toBeNull();
  });

  it("没有原始正文的事件不给切换", async () => {
    mount("/sources/events/sev_chat");
    const col = await column();
    await col.findByRole("list", { name: "会话消息" });
    expect(col.queryByRole("button", { name: "原始格式" })).toBeNull();
  });
});

describe("页头写明四种完整程度（ACT-WRP-03）", () => {
  it("partial 写明没取到哪一部分与原因", async () => {
    mount("/sources/events/sev_partial");
    const col = await column();
    expect(await col.findByText(/来源程序没能取到全文：正文（mail server timed out）/)).toBeInTheDocument();
    expect(col.getByText("只有开头")).toBeInTheDocument();
  });
  it("legacy 写升级前落盘、可能已截断", async () => {
    mount("/sources/events/sev_legacy");
    const col = await column();
    expect(await col.findByText(/升级前落盘，当时可能已被来源程序截断/)).toBeInTheDocument();
    expect(col.getByText("旧事件正文")).toBeInTheDocument();
  });
  it("purged 写已按保留期清除，不显示任何部分", async () => {
    mount("/sources/events/sev_purged");
    const col = await column();
    expect(await col.findByText(/已按保留期清除/)).toBeInTheDocument();
    expect(col.queryByRole("table")).toBeNull();
    expect(col.queryByRole("button", { name: "原始格式" })).toBeNull();
  });
});

describe("送给模型 N / M 字符（ACT-BUD-05）", () => {
  it("有裁剪量的处置：判断级与处置行写送了多少、原文多少", async () => {
    mount("/sources/events/sev_mail");
    const list = await screen.findByRole("list", { name: "处理经过" });
    const rows = within(list).getAllByRole("listitem").map((li) => li.textContent);
    expect(rows[1]).toContain("送给模型 8,000 / 20,000 字符");
    expect(rows[2]).toContain("送给模型 24,000 / 61,000 字符");
  });

  it("升级前的旧处置没有 material：不显示、不报错、不伪造", async () => {
    mount("/sources/events/sev_old");
    const list = await screen.findByRole("list", { name: "处理经过" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    expect(list.textContent).not.toMatch(/送给模型/);
    expect(await screen.findByRole("region", { name: "原文" })).toBeInTheDocument();
  });
});

// ---- 取回与物理删除（ACT-FET-02 / flow-raw-purge）：替身按 POST /source-events/{id}/attachments/{n}/fetch 与 POST /settings/purge 的契约应答 ----
describe("附件取回与来源事件物理删除", () => {
  const FETCHABLE = {
    ...MAIL,
    event: ev("sev_fetch"),
    attachments: [
      { name: "合同.pdf", media_type: "application/pdf", size: 1024, ref: "h1" },
      { name: "图.png", media_type: "image/png", size: 2048, ref: "h2", blob: "apa://blob/ab" },
      { name: "外链.zip", size: 5 },
    ],
  };
  let calls: { method: string; url: string; body?: unknown }[] = [];
  let fetchReply: { status: number; body: unknown } = { status: 200, body: { name: "合同.pdf", media_type: "application/pdf", size: 1024, ref: "h1", blob: "apa://blob/cd" } };
  const server = (() =>
    (async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? "GET";
      calls.push({ method, url, body: init.body ? JSON.parse(init.body as string) : undefined });
      if (url === "/v1/projects") return json(200, { projects: [] });
      if (url === "/v1/sources") return json(200, { sources: [] });
      if (url === "/v1/source-events/sev_nofetch/trace") return json(200, trace("sev_nofetch", [arrived]));
      if (url === "/v1/source-events/sev_nofetch") return json(200, NOFETCH);
      if (url === "/v1/source-events/sev_fetch/trace") return json(200, trace("sev_fetch", [arrived]));
      if (url === "/v1/source-events/sev_fetch") return json(200, FETCHABLE);
      if (url === "/v1/source-events/sev_fetch/attachments/0/fetch" && method === "POST") return json(fetchReply.status, fetchReply.body);
      if (url === "/v1/settings/purge" && method === "POST") return json(200, { object: "sev_fetch", kind: "source_event", events: 2 });
      return json(404, { code: "not_found", why: "no such route", next: "" });
    }) as unknown as typeof fetch)();
  const NOFETCH = { ...MAIL, event: ev("sev_nofetch"), attachments: [{ name: "report.pdf", media_type: "application/pdf", size: 1024 }, { name: "logo.png", media_type: "image/png", size: 2048 }] };
  const open = (id = "sev_fetch") => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    render(
      <MemoryRouter initialEntries={["/sources/events/" + id]}>
        <App features={[sources]} fetchImpl={server} />
      </MemoryRouter>,
    );
  };
  beforeEach(() => {
    calls = [];
    fetchReply = { status: 200, body: { name: "合同.pdf", media_type: "application/pdf", size: 1024, ref: "h1", blob: "apa://blob/cd" } };
  });

  it("只有带取回句柄且未取回的附件有取回按钮；不支持的只显示名字类型大小", async () => {
    open();
    const col = await column();
    const att = await col.findByRole("table", { name: "附件" });
    expect(within(within(att).getByRole("row", { name: /合同\.pdf/ })).getByRole("button", { name: /取回/ })).toBeInTheDocument();
    expect(within(within(att).getByRole("row", { name: /图\.png/ })).queryByRole("button")).toBeNull();
    expect(within(att).getByRole("row", { name: /图\.png.*已取回/ })).toBeInTheDocument();
    expect(within(within(att).getByRole("row", { name: /外链\.zip/ })).queryByRole("button")).toBeNull();
  });

  it("来源不支持取回（读接口不带 ref，如 acc-raw-nofetch）：整张附件表没有取回按钮，只有名字、类型与大小", async () => {
    open("sev_nofetch");
    const col = await column();
    const att = await col.findByRole("table", { name: "附件" });
    expect(within(att).getByRole("row", { name: /report\.pdf\s*application\/pdf\s*1\.0 KiB/ })).toBeInTheDocument();
    expect(within(att).queryByRole("button")).toBeNull();
  });

  it("取回成功：POST 第 n 项，该行改为已取回", async () => {
    open();
    const col = await column();
    await userEvent.click(await col.findByRole("button", { name: "取回 合同.pdf" }));
    expect(calls.some((c) => c.method === "POST" && c.url === "/v1/source-events/sev_fetch/attachments/0/fetch")).toBe(true);
    expect(await col.findByRole("row", { name: /合同\.pdf.*已取回/ })).toBeInTheDocument();
    expect(col.queryByRole("button", { name: "取回 合同.pdf" })).toBeNull();
  });

  it.each([[409, "the source program does not support fetching attachments"], [502, "attachment is larger than 32 MiB"]])("取回失败 %i：显示原因，清单不变，仍可重试", async (status, why) => {
    fetchReply = { status, body: { code: status === 409 ? "conflict" : "upstream_failed", why, next: "open the original message instead" } };
    open();
    const col = await column();
    await userEvent.click(await col.findByRole("button", { name: "取回 合同.pdf" }));
    expect((await col.findAllByText(new RegExp(why))).length).toBeGreaterThan(0);
    expect(col.getByRole("button", { name: "取回 合同.pdf" })).toBeEnabled();
    expect(col.queryByRole("row", { name: /合同\.pdf.*已取回/ })).toBeNull();
  });

  it("物理删除：确认前不发请求；取消不删；确认后 POST /settings/purge {object: 事件 id} 并显示已删除", async () => {
    open();
    const col = await column();
    await userEvent.click(await col.findByRole("button", { name: "删除这条来源事件…" }));
    expect(screen.getByRole("alertdialog")).toHaveTextContent("无法找回");
    expect(calls.some((c) => c.url === "/v1/settings/purge")).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "取消" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(calls.some((c) => c.url === "/v1/settings/purge")).toBe(false);
    await userEvent.click(col.getByRole("button", { name: "删除这条来源事件…" }));
    await userEvent.click(screen.getByRole("button", { name: "确认永久删除" }));
    expect(calls.find((c) => c.url === "/v1/settings/purge")?.body).toEqual({ object: "sev_fetch" });
    expect(await col.findByText("这条来源事件已永久删除")).toBeInTheDocument();
    expect(col.queryByRole("table", { name: "附件" })).toBeNull();
  });
});
