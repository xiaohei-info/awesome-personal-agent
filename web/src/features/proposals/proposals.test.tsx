import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as decisions } from "../decisions";
import { feature } from "./index";

// 测试边界替身：按 api/items（phase 过滤）与 api/decisions（accept/dismiss）的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface P { id: string; phase: string; title: string; proposal?: { recommend?: string; attach_to?: string; reason?: string; low_confidence: boolean; sources?: string[] }; summary?: string; project?: string; priority?: string; origin?: { source: string; actor?: string }; ask?: unknown }
const prop = (id: string, over: Partial<P> = {}): P => ({ id, phase: "proposed", title: "建议 " + id, ...over });

function server(initial: P[], write?: (path: string, body: any) => Response | undefined, rejections: Record<string, unknown> = {}) {
  let items = initial;
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_mail_qq", name: "QQ 邮箱" }] });
    const q = url.match(/^\/v1\/items\?phase=(\w+)$/);
    if (q) return json(200, { items: items.filter((i) => i.phase === q[1]), last_rejections: rejections });
    if (url === "/v1/proposals/batch" && method === "POST") {
      const res = write?.(url, body);
      if (res) return res;
      return json(200, { results: [] });
    }
    const w = url.match(/^\/v1\/items\/(.+)\/(accept|dismiss|attach)$/);
    if (w && method === "POST") {
      const res = write?.(url, body);
      if (res) return res;
      items = items.filter((i) => i.id !== w[1]);
      return json(200, { id: w[1], version: 2, replayed: false });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch, path = "/proposals") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App features={[decisions, feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

const rej = (over: object = {}) => ({ seq: 9, at: "2030-01-01T00:00:00Z", actor: { type: "agent", principal: "DEL", via: "cli" }, code: "basis_required", why: "the grant does not cover this action", grant: "grt_bad", ...over });


describe("建议流页（flow-proposal-triage）", () => {
  it("ACT-PROP-01：只列 proposed 事项，请求 phase=proposed，不与决策待办混在一起", async () => {
    const s = server([prop("a", { project: "acc-open", origin: { source: "agent", actor: "observer" } }), prop("n", { phase: "needs_input", title: "决策 n" })]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    expect(within(card).getByText(/acc-open/)).toBeInTheDocument();
    expect(screen.queryByText("决策 n")).not.toBeInTheDocument();
    expect(s.calls.some((c) => c.path === "/v1/items?phase=proposed")).toBe(true);
    // 导航上的待决数会读一次 needs_input；建议流页自己不读
    expect(s.calls.filter((c) => c.path === "/v1/items?phase=needs_input").length).toBeLessThanOrEqual(1);
  });

  it("ACT-PROP-01：决策待办页不出现建议", async () => {
    const s = server([prop("a"), prop("n", { phase: "needs_input", title: "决策 n", ask: { question: "问 n", options: [] } })]);
    mount(s.impl, "/decisions");
    await screen.findByText("问 n");
    expect(screen.queryByText("建议 a")).not.toBeInTheDocument();
  });

  it("ACT-PROP-03：接受 → POST accept 带 CSRF，离开建议流", async () => {
    const s = server([prop("a"), prop("b")]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "接受" }));
    await screen.findByText("建议 b");
    expect(screen.queryByText("建议 a")).not.toBeInTheDocument();
    const w = s.calls.find((c) => c.method === "POST" && c.path === "/v1/items/a/accept")!;
    expect(w.csrf).toBe("tok-1");
  });

  it("ACT-PROP-04：忽略的理由选填；有理由时随 dismiss 提交，离开建议流", async () => {
    const s = server([prop("a"), prop("b")]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "忽略" }));
    await userEvent.type(within(card).getByLabelText("忽略原因（选填）"), "重复");
    await userEvent.click(within(card).getByRole("button", { name: "确认忽略" }));
    await screen.findByText("建议 b");
    expect(screen.queryByText("建议 a")).not.toBeInTheDocument();
    expect(s.calls.find((c) => c.path === "/v1/items/a/dismiss")!.body).toEqual({ reason: "重复" });
    expect(s.calls.some((c) => c.path === "/v1/items/a/accept")).toBe(false);
  });

  it("ACT-PROP-04：不填理由也能忽略，请求体不带 reason", async () => {
    const s = server([prop("a"), prop("b")]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "忽略" }));
    await userEvent.type(within(card).getByLabelText("忽略原因（选填）"), "   ");
    await userEvent.click(within(card).getByRole("button", { name: "确认忽略" }));
    await screen.findByText("建议 b");
    expect(s.calls.find((c) => c.path === "/v1/items/a/dismiss")!.body).toEqual({});
  });

  it("已被别处处理（409）：显示服务端原因并刷新列表", async () => {
    const s = server([prop("a")], () => json(409, { code: "conflict", why: "the item is not a pending proposal", next: "re-read the item; the first answer stands" }));
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "接受" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not a pending proposal");
    expect(s.calls.filter((c) => c.path === "/v1/items?phase=proposed").length).toBeGreaterThan(1);
  });

  it("空列表", async () => {
    mount(server([]).impl);
    expect(await screen.findByText("建议都处理完了")).toBeInTheDocument();
  });

  it("读取失败：显示错误并可重试", async () => {
    let fail = true;
    const s = server([prop("a")]);
    const impl = ((url: string, init?: RequestInit) =>
      fail && url.startsWith("/v1/items") ? Promise.resolve(json(500, { code: "internal", why: "boom", next: "retry" })) : s.impl(url, init)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("建议 a")).toBeInTheDocument();
  });
});

describe("被拒绝的处理（ACT-PROP-05、flow-proposal-triage）", () => {
  it("标明这次被拒绝的处理与原因；阶段不变仍可由本人接受", async () => {
    const s = server([prop("a"), prop("b")], undefined, { a: rej({ why: "no basis for dismiss" }) });
    mount(s.impl);
    const a = await screen.findByRole("article", { name: "建议 a" });
    const note = within(a).getByRole("note", { name: "被拒绝的处理" });
    expect(note).toHaveTextContent("no basis for dismiss");
    expect(within(note).getByRole("link", { name: "去放开它的档位" })).toHaveAttribute("href", "/settings") // 档位在「我来拍板」;
    expect(within(screen.getByRole("article", { name: "建议 b" })).queryByRole("note")).toBeNull();
    expect(within(a).getByRole("button", { name: "接受" })).toBeEnabled();
  });
});

describe("建议流：推荐、理由、来源数与把握低（ACT-PROP-10、ACT-PRH-07）", () => {
  it("ACT-PRH-07：显示推荐处置、理由、来源数；把握低的有标记，没有推荐的不显示推荐标记", async () => {
    const s = server([
      prop("r", { proposal: { recommend: "dismiss", reason: "群发通知，与你无关", low_confidence: false, sources: ["prh-rec-dismiss"] } }),
      prop("u", { proposal: { low_confidence: true, sources: ["e1", "e2"] } }),
      prop("d"),
    ]);
    mount(s.impl);
    const r = await screen.findByRole("article", { name: "建议 r" });
    expect(within(r).getByText(/推荐：忽略/)).toBeInTheDocument();
    expect(within(r).getByText(/群发通知，与你无关/)).toBeInTheDocument();
    expect(within(r).getByText("1 条消息")).toBeInTheDocument();
    expect(within(r).queryByText("把握低")).toBeNull();
    const u = screen.getByRole("article", { name: "建议 u" });
    expect(within(u).getByText("把握低")).toBeInTheDocument();
    expect(within(u).getByText("2 条消息")).toBeInTheDocument();
    expect(within(u).queryByText(/推荐：/)).toBeNull();
    expect(within(screen.getByRole("article", { name: "建议 d" })).queryByText(/推荐：/)).toBeNull();
  });

  it("ACT-PROP-10：每条有接受、补到已有事项、忽略三个操作，列表顶部有「按推荐处理」", async () => {
    mount(server([prop("d")]).impl);
    const d = await screen.findByRole("article", { name: "建议 d" });
    for (const n of ["接受", "补到已有事项", "忽略"]) expect(within(d).getByRole("button", { name: n })).toBeEnabled();
    expect(screen.getByRole("button", { name: /^按推荐处理 \d+ 条$/ })).toBeInTheDocument();
  });
});

describe("补到已有事项（ACT-PRH-10）", () => {
  it("候选取自 open 事项；选定确认 → POST attach {to}，建议离开建议流", async () => {
    const s = server([prop("a"), prop("b"), prop("t3", { phase: "open", title: "T3 事项" }), prop("t9", { phase: "done", title: "已完成事项" })]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "补到已有事项" }));
    expect(await within(card).findByRole("option", { name: "T3 事项" })).toBeInTheDocument();
    expect(within(card).queryByRole("option", { name: "已完成事项" })).toBeNull();
    const confirm = within(card).getByRole("button", { name: "确认补到" });
    expect(confirm).toBeDisabled();
    await userEvent.selectOptions(within(card).getByLabelText("补到哪个事项"), "t3");
    await userEvent.click(confirm);
    await screen.findByText("建议 b");
    expect(screen.queryByText("建议 a")).not.toBeInTheDocument();
    const w = s.calls.find((c) => c.path === "/v1/items/a/attach")!;
    expect(w.body).toEqual({ to: "t3" });
    expect(w.csrf).toBe("tok-1");
  });

  it("推荐补到某事项时默认选中该候选", async () => {
    const s = server([prop("a", { proposal: { recommend: "attach", attach_to: "t3", reason: "同一件事", low_confidence: false, sources: ["e"] } }), prop("t3", { phase: "open", title: "T3 事项" })]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "补到已有事项" }));
    await within(card).findByRole("option", { name: "T3 事项" });
    expect(within(card).getByLabelText("补到哪个事项")).toHaveValue("t3");
  });

  it("目标已终结（409）：显示服务端原因，建议仍在", async () => {
    const s = server([prop("a"), prop("t3", { phase: "open", title: "T3 事项" })], (u) => (u.endsWith("/attach") ? json(409, { code: "conflict", why: "the target item is closed", next: "pick another" }) : undefined));
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "补到已有事项" }));
    await userEvent.selectOptions(await within(card).findByLabelText("补到哪个事项"), "t3");
    await userEvent.click(within(card).getByRole("button", { name: "确认补到" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("target item is closed");
    expect(screen.getByText("建议 a")).toBeInTheDocument();
  });
});

describe("按推荐批量处理（ACT-PRH-13、ACT-PRH-14）", () => {
  const rec = (recommend: string) => ({ proposal: { recommend, low_confidence: false, sources: ["e"] } });

  it("按推荐处理前先看到条数与各处置的分布；没有推荐的不计入", async () => {
    mount(server([prop("a1", rec("accept")), prop("a2", rec("accept")), prop("d1", rec("dismiss")), prop("n1")]).impl);
    await screen.findByText("建议 a1");
    await userEvent.click(screen.getByRole("button", { name: "按推荐处理 3 条" }));
    expect(screen.getByText("接受 2 条 · 忽略 1 条")).toBeInTheDocument();
  });

  it("ACT-PRH-13：按来源与项目筛选后一次点击，确认后 POST batch {mode:recommended, filter}；filter 是筛选加上眼前可见且带推荐的建议 id，不逐条列动作", async () => {
    const s = server([
      prop("b1", { origin: { source: "PRH_A" }, project: "acc-open", ...rec("accept") }),
      prop("b4", { origin: { source: "PRH_A" }, project: "acc-open" }),
      prop("x", { origin: { source: "OTHER" }, project: "other", ...rec("accept") }),
    ]);
    mount(s.impl);
    await screen.findByText("建议 b1");
    await userEvent.selectOptions(screen.getByLabelText("来源"), "PRH_A");
    await userEvent.selectOptions(screen.getByLabelText("项目"), "acc-open");
    expect(screen.queryByText("建议 x")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^按推荐处理 \d+ 条$/ }));
    expect(s.calls.some((c) => c.path === "/v1/proposals/batch")).toBe(false); // 先确认
    await userEvent.click(screen.getByRole("button", { name: "确认按推荐处理" }));
    await waitFor(() => expect(s.calls.some((c) => c.path === "/v1/proposals/batch")).toBe(true));
    const b = s.calls.find((c) => c.path === "/v1/proposals/batch")!;
    expect(b.body).toEqual({ mode: "recommended", filter: { source: "PRH_A", project: "acc-open", ids: ["b1"] } });
    expect(b.csrf).toBe("tok-1");
  });

  it("来源按名字称呼（§9.1）：卡片的「来自」与来源筛选都显示名字，对话捕获显示「对话捕获 · 项目」；不出现 src_", async () => {
    mount(server([prop("m", { origin: { source: "src_mail_qq" } }), prop("c", { origin: { source: "capture:acc-open" } })]).impl);
    await screen.findByText("建议 m");
    await waitFor(() => expect(document.querySelector("main")!.textContent).toContain("QQ 邮箱"));
    expect(document.querySelector("main")!.textContent).toContain("对话捕获 · acc-open");
    const pick = screen.getByLabelText("来源");
    expect(within(pick).getByRole("option", { name: "QQ 邮箱" })).toHaveValue("src_mail_qq");
    expect(document.querySelector("main")!.textContent, "主界面不出现来源 id").not.toMatch(/src_/);
  });

  it("ACT-PRH-14：目标已关闭的那条跳过并说明原因，成功的计数，列表以服务端为准刷新", async () => {
    const s = server(
      [prop("s1", { title: "建议 skip-1", ...rec("attach") }), prop("s2", { title: "建议 skip-2", ...rec("accept") })],
      (u) =>
        u === "/v1/proposals/batch"
          ? json(200, { results: [{ id: "s1", do: "attach", ok: false, code: "conflict", why: "the target item is closed" }, { id: "s2", do: "accept", ok: true }] })
          : undefined,
    );
    mount(s.impl);
    await screen.findByText("建议 skip-1");
    await userEvent.click(screen.getByRole("button", { name: /^按推荐处理 \d+ 条$/ }));
    await userEvent.click(screen.getByRole("button", { name: "确认按推荐处理" }));
    const res = await screen.findByRole("status", { name: "批量处理结果" });
    expect(res).toHaveTextContent("已处理 1 条");
    expect(res).toHaveTextContent("跳过 1 条");
    expect(res).toHaveTextContent("建议 skip-1");
    expect(res).toHaveTextContent("the target item is closed");
    expect(s.calls.filter((c) => c.path === "/v1/items?phase=proposed").length).toBeGreaterThan(1);
  });

  it("整批被拒（如缺依据）：显示服务端原因，不报告成功", async () => {
    const s = server([prop("a", rec("accept"))], (u) => (u === "/v1/proposals/batch" ? json(403, { code: "permission_denied", why: "batch needs the owner's own words", next: "use the web session" }) : undefined));
    mount(s.impl);
    await screen.findByText("建议 a");
    await userEvent.click(screen.getByRole("button", { name: /^按推荐处理 \d+ 条$/ }));
    await userEvent.click(screen.getByRole("button", { name: "确认按推荐处理" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("batch needs the owner's own words");
    expect(screen.queryByRole("status", { name: "批量处理结果" })).toBeNull();
  });

  it("范围内没有任何带推荐的建议时，「按推荐处理」不可点", async () => {
    mount(server([prop("d")]).impl);
    await screen.findByText("建议 d");
    expect(screen.getByRole("button", { name: /^按推荐处理 \d+ 条$/ })).toBeDisabled();
  });
});

// 主流程路径一（界面设计 §7）：建议卡上直接「接受并交给 Agent」。
describe("接受并交给 Agent", () => {
  const RULE = "background-run/event_rules/handle-labelled";
  function wrap(mode: string, ready: boolean) {
    const s = server([{ ...prop("a"), version: 4, tags: ["mine"] } as P, prop("b")]);
    const order: string[] = [];
    const impl = (async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? "GET";
      if (url === "/v1/rules") return json(200, { rules: [{ key: RULE, mode }] });
      if (url.startsWith("/v1/agents?")) return json(200, { agents: [], selected: ready ? { agent_id: "agt_1", agent_name: "小黑" } : {}, reason: ready ? "" : "还没选主 Agent" });
      if (method !== "GET") order.push(method + " " + url + (init.body ? " " + init.body : "") + ((init.headers as Record<string, string>)?.["If-Match"] ? " If-Match:" + (init.headers as Record<string, string>)["If-Match"] : ""));
      if (method === "PATCH") return json(200, { id: "a", version: 5 });
      return s.impl(url, init);
    }) as unknown as typeof fetch;
    return { impl, order };
  }

  it("前提都满足：先给建议加上「交给 Agent」再接受；卡片收成回执，链到事项的 Agent 处理", async () => {
    const w = wrap("auto", true);
    mount(w.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await waitFor(() => expect(within(card).getByRole("button", { name: "接受并交给 Agent" })).toHaveClass("primary"));
    await userEvent.click(within(card).getByRole("button", { name: "接受并交给 Agent" }));
    const receipt = (await screen.findByText(/已接受 · 已交给 小黑，排队中/)).closest("li")!;
    expect(within(receipt).getByRole("link", { name: "查看事项" })).toHaveAttribute("href", "/items/a#agent");
    expect(w.order).toEqual([
      'PATCH /v1/items/a {"tags":["mine","agent-ok"],"reason":"交给 Agent 处理"} If-Match:4',
      "POST /v1/items/a/accept {}",
    ]);
  });

  it("前提没满足：只接受、不交出，回执写明，并列出还差的几步", async () => {
    const w = wrap("observe", true);
    mount(w.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "接受并交给 Agent" }));
    const receipt = (await screen.findByText(/已接受，还没交给 Agent/)).closest("li")!;
    expect(within(receipt).getByRole("link", { name: "去打开" })).toHaveAttribute("href", "/rules#rule-" + encodeURIComponent(RULE));
    expect(w.order).toEqual(["POST /v1/items/a/accept {}"]);
  });
});

describe("跨来源合并：展开看来源与依据，一键拆分（§9.3）", () => {
  it("展开列出每个来源的名字、时间、原话与合并依据；点「拆出来」发 POST /source-events/{id}/split，回执指向新建议", async () => {
    const merged = prop("m", { origin: { source: "src_mail_qq" }, proposal: { low_confidence: false, sources: ["e1", "e2"],
      merges: [{ event: "e2", source: "src_wx", title: "AgentSafe 资产确认", by: "classifier", score: 0.72, judgment: "jdg_1", at: "2030-01-01T00:00:00Z" }] } as any });
    const s = server([merged]);
    const base = s.impl;
    const impl = (async (url: string, init: RequestInit = {}) => {
      if (url === "/v1/sources") return json(200, { sources: [{ id: "src_mail_qq", name: "QQ 邮箱" }, { id: "src_wx", name: "微信" }] });
      if (url === "/v1/source-events/e1") return json(200, { event: { id: "e1", source_id: "src_mail_qq", subject: "确认 AgentSafe AI 资产", occurred_at: "2030-01-01T00:00:00Z" } });
      if (url === "/v1/source-events/e2") return json(200, { event: { id: "e2", source_id: "src_wx", subject: "AgentSafe", occurred_at: "2030-01-01T01:00:00Z" } });
      if (url === "/v1/source-events/e2/split") { (base as any)(url, init); return json(200, { item: "n1" }); }
      return base(url, init);
    }) as unknown as typeof fetch;
    mount(impl);
    const card = await screen.findByRole("article", { name: "建议 m" });
    await userEvent.click(within(card).getByText("2 条消息 → 这一条"));
    const wx = await within(card).findByRole("listitem", { name: "微信" });
    expect(wx).toHaveTextContent("打分模型判断为同一件事，自动合并 · 标题相似度 0.72 · 合并于");
    expect(within(card).getByRole("listitem", { name: "QQ 邮箱" })).toHaveTextContent("最先到，建出这条");
    expect(within(within(card).getByRole("listitem", { name: "QQ 邮箱" })).queryByRole("button", { name: "拆出来" })).toBeNull();
    await userEvent.click(within(wx).getByRole("button", { name: "拆出来" }));
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/source-events/e2/split")).toMatchObject({ method: "POST", body: { from: "m" }, csrf: "tok-1" }));
    expect(await screen.findByText(/已拆成单独的一条建议/)).toBeInTheDocument();
    expect(document.querySelector("main")!.textContent, "界面不出现来源 id").not.toMatch(/src_/);
  });
});

describe("跨来源合并的批量归类：过程可追溯、可撤销（§9.3）", () => {
  const merged = prop("m", { origin: { source: "src_mail_qq" }, proposal: { low_confidence: false, sources: ["e1", "e2"],
    merges: [{ event: "e2", source: "src_wx", title: "AgentSafe 资产确认", by: "generate", score: 0.81, why: "同一个资产编号 B18630823", batch: "mb_1", item: "f1", at: "2030-01-01T02:00:00Z" }] } as any });
  const activity = { days: 7, pending: 3, model_calls: 1, chars: 4200, batches: [{ batch: "mb_1", at: "2030-01-01T02:00:00Z", size: 3, run: "run_1", chars: 4200, decisions: [
    { item: "f1", title: "AgentSafe 资产确认", source: "src_wx", sources: 1, action: "merge", target: "m", target_title: "建议 m", why: "同一个资产编号 B18630823", by: "generate" },
    { item: "f2", title: "周报模板改版", source: "src_dx", sources: 1, action: "duplicate", target: "d9", target_title: "周报模板改版 W47", why: "本人忽略过的同一件事", by: "generate" },
    { item: "f3", title: "导出报表加一列", source: "src_wx", action: "keep", why: "和账本里已有的记录比过，没有相似的", by: "rule" }] },
    { batch: "mb_2", at: "2030-01-01T01:00:00Z", size: 20, run: "run_2", chars: 5900, deferred: 9, decisions: [] }] };
  function withMerge() {
    const s = server([merged]);
    const base = s.impl;
    const impl = (async (url: string, init: RequestInit = {}) => {
      if (url === "/v1/sources") return json(200, { sources: [{ id: "src_mail_qq", name: "QQ 邮箱" }, { id: "src_wx", name: "微信" }, { id: "src_dx", name: "大象" }] });
      if (url === "/v1/merge/activity?days=7") return json(200, activity);
      if (url === "/v1/source-events/e1") return json(200, { event: { id: "e1", source_id: "src_mail_qq", subject: "确认 AgentSafe AI 资产", occurred_at: "2030-01-01T00:00:00Z", link: "https://mail.example/1" } });
      if (url === "/v1/source-events/e2") return json(200, { event: { id: "e2", source_id: "src_wx", subject: "AgentSafe", summary: "@我 资产 B18630823 要确认", occurred_at: "2030-01-01T01:00:00Z" } });
      if (url === "/v1/items/f1") return json(200, { item: { id: "f1", phase: "dropped", title: "AgentSafe 资产确认", proposal: { low_confidence: false, sources: ["e2"] } } });
      if (url.endsWith("/unmerge")) { (base as any)(url, init); return json(200, { item: url.split("/")[3] }); }
      return base(url, init);
    }) as unknown as typeof fetch;
    return { impl, calls: s.calls };
  }

  it("展开来源：每条的来源、时间、原话或链接，合并时刻、判断方与理由；「撤销合并」发 POST /items/{被折叠的那条}/unmerge", async () => {
    const s = withMerge();
    mount(s.impl);
    const card = await screen.findByRole("article", { name: "建议 m" });
    await userEvent.click(within(card).getByText("2 条消息 → 这一条"));
    // 批量归类折进来的那条（f1）整组列出：组头写它连同几条消息并进这条、哪一批、谁判断、相似度与理由，组里是它带来的消息。
    const fold = await within(card).findByRole("listitem", { name: "「AgentSafe 资产确认」并进来的几条" });
    expect(fold).toHaveTextContent("「AgentSafe 资产确认」连同 1 条消息 → 这条");
    expect(fold).toHaveTextContent("那批自动归类 · 写作模型判断为同一件事，自动合并（与生成事项和建议相同） · 标题相似度 0.81");
    expect(fold).toHaveTextContent("理由：同一个资产编号 B18630823");
    const wx = within(fold).getByRole("listitem", { name: "微信" });
    expect(wx).toHaveTextContent("@我 资产 B18630823 要确认");
    expect(wx).toHaveTextContent("最先到，建出「AgentSafe 资产确认」");
    expect(within(card).getByRole("listitem", { name: "QQ 邮箱" })).toHaveTextContent("最先到，建出这条 · 原文 · 链接");
    await userEvent.click(within(fold).getByRole("button", { name: "撤销合并" }));
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/items/f1/unmerge")).toMatchObject({ method: "POST", csrf: "tok-1" }));
    expect(await screen.findByText(/已撤销合并，它回到了建议里/)).toBeInTheDocument();
  });

  it("从哪几条合来：展开列出 proposal.absorbed 的原始标题、来源、建立时间与怎么合的；没有消息的迁移建议也列；撤销合并后随之消失", async () => {
    let absorbed = [
      { item: "itm_v", title: "整理报销单据（清单）", source: "agent:vault-migrate", ref: "todo-migrate:abc", created_at: "2030-01-01T00:00:00Z", action: "duplicate", at: "2030-01-02T00:00:00Z" },
      { item: "itm_g", title: "fix: 报销导出", source: "src_mail_qq", ref: "https://git.example/org/web/pull/7", created_at: "2030-01-01T02:00:00Z", action: "merge", at: "2030-01-02T00:00:00Z" },
    ];
    const s = server([]);
    const impl = (async (url: string, init: RequestInit = {}) => {
      if (url === "/v1/items?phase=proposed") return json(200, { items: [prop("t", { proposal: { low_confidence: false, absorbed } as any })], last_rejections: {} });
      if (url === "/v1/items/itm_v/unmerge") { (s.impl as any)(url, init); absorbed = absorbed.filter((a) => a.item !== "itm_v"); return json(200, { item: "itm_v" }); }
      return s.impl(url, init);
    }) as unknown as typeof fetch;
    mount(impl);
    const card = await screen.findByRole("article", { name: "建议 t" });
    await userEvent.click(within(card).getByText("从 2 条建议合来"));
    const v = within(card).getByRole("listitem", { name: "整理报销单据（清单）" });
    expect(v).toHaveTextContent("agent:vault-migrate");
    expect(v).toHaveTextContent("建于 " + new Date("2030-01-01T00:00:00Z").toLocaleString());
    expect(v).toHaveTextContent("判定重复 · 合并于 " + new Date("2030-01-02T00:00:00Z").toLocaleString());
    const g = within(card).getByRole("listitem", { name: "fix: 报销导出" });
    expect(g).toHaveTextContent("QQ 邮箱");
    expect(within(g).getByRole("link", { name: "链接" })).toHaveAttribute("href", "https://git.example/org/web/pull/7");
    await userEvent.click(within(v).getByRole("button", { name: "撤销合并" }));
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/items/itm_v/unmerge")).toMatchObject({ method: "POST", csrf: "tok-1" }));
    await waitFor(() => expect(within(screen.getByRole("article", { name: "建议 t" })).queryByText("整理报销单据（清单）")).toBeNull());
    expect(screen.getByRole("article", { name: "建议 t" })).toHaveTextContent("从 1 条建议合来");
  });

  it("最近自动归类：写明每批几条、几次模型调用、送出多少字，每条的去处与理由；折叠掉的可撤销，留着的不给撤销", async () => {
    mount(withMerge().impl);
    const panel = await screen.findByRole("group", { name: "最近自动归类" }).catch(() => null) ?? (await screen.findByText(/最近 7 天自动归类/)).closest("details")!;
    expect(panel).toHaveTextContent("合成一条建议 1 条、判定重复 1 条、留着 1 条 · 模型调用 1 次，送出约 4200 字 · 还有 3 条等着成批");
    await userEvent.click(within(panel as HTMLElement).getByText(/最近 7 天自动归类/));
    expect(panel).toHaveTextContent("同批 3 条，一次模型调用，送出约 4200 字");
    expect(within(panel as HTMLElement).getByText(/同批 3 条/)).not.toHaveTextContent("砍半");
    expect(panel, "超出资料上限砍半的那批写明留到下一轮的条数").toHaveTextContent("同批 20 条，一次模型调用，送出约 5900 字；资料超出上限，这批砍半，9 条留到下一轮");
    // 折叠掉的按去处收成「N 条 → 「去处」」，组里逐条写结论、判断方与理由。
    expect(within(panel as HTMLElement).getByRole("listitem", { name: "合进「周报模板改版 W47」" })).toHaveTextContent("1 条建议（1 条消息）→ 「周报模板改版 W47」");
    const dup = within(panel as HTMLElement).getByRole("listitem", { name: "周报模板改版" });
    expect(dup).toHaveTextContent("周报模板改版 · 大象 · 1 条消息判定重复 · 写作模型判断");
    expect(dup).toHaveTextContent("理由：本人忽略过的同一件事");
    expect(within(dup).getByRole("button", { name: "撤销合并" })).toBeInTheDocument();
    expect(within(panel as HTMLElement).queryByRole("listitem", { name: "导出报表加一列" }), "规则比过留着的只记条数").toBeNull();
    expect(panel).toHaveTextContent("另有 1 条规则比过没有相似的，留着");
    expect(document.querySelector("main")!.textContent, "界面不出现来源 id").not.toMatch(/src_|mb_|run_/);
  });
});

describe("合并结果写成「N 条消息 → 这一条」（线上 2026-11-23 PR #227 那批的形状）", () => {
  const names = [{ id: "src_mail_icloud", name: "iCloud 邮件" }, { id: "src_mail", name: "邮件" }, { id: "src_github", name: "GitHub" }];
  const at = (m: number) => `2030-01-01T06:${String(m).padStart(2, "0")}:00Z`;
  const ev = (id: string, source_id: string, subject: string, m: number, over: object = {}) => ({ id, source_id, subject, occurred_at: at(m), ...over });
  function mountWith(items: P[], events: Record<string, object>, extra: Record<string, unknown> = {}) {
    const s = server(items);
    const impl = (async (url: string, init: RequestInit = {}) => {
      if (url === "/v1/sources") return json(200, { sources: names });
      const e = url.match(/^\/v1\/source-events\/([^/]+)$/);
      if (e) return json(200, { event: events[e[1]] });
      if (url in extra) return json(200, extra[url]);
      if (url.endsWith("/unmerge")) { (s.impl as any)(url, init); return json(200, { item: url.split("/")[3] }); }
      return s.impl(url, init);
    }) as unknown as typeof fetch;
    mount(impl);
    return s;
  }
  const noIds = () => expect(document.querySelector("main")!.textContent, "界面不出现裸 id").not.toMatch(/itm_|sev_|src_|mb_|run_|jdg_/);

  it("改后：6 条在生成时就按同一链接合成 1 条，逐条写来源、时间、怎么进来的、相似度与理由，没有折叠组", async () => {
    const key = { merge_key: "link:https://github.com/org/app/pull/227", link: "https://github.com/org/app/pull/227" };
    const rule = (event: string, source: string, m: number) => ({ event, source, title: "评审 PR #227", by: "merge_key", at: at(m) });
    const y = prop("itm_y", { title: "审阅 PR #227", origin: { source: "src_mail_icloud" }, proposal: { low_confidence: false,
      sources: ["sev_1", "sev_2", "sev_3", "sev_4", "sev_5", "sev_6"],
      merges: [rule("sev_2", "src_mail_icloud", 8), rule("sev_3", "src_mail", 21), rule("sev_6", "src_github", 29)] } as any });
    mountWith([y], {
      sev_1: ev("sev_1", "src_mail_icloud", "Re: [org/app] 卡片移到最上方 (PR #227)", 0, key),
      sev_2: ev("sev_2", "src_mail_icloud", "[org/app] 卡片移到最上方 (PR #227)", 0, key),
      sev_3: ev("sev_3", "src_mail", "[org/app] 卡片移到最上方 (PR #227)", 0, key),
      sev_4: ev("sev_4", "src_mail", "Re: [org/app] 卡片移到最上方 (PR #227)", 0, { ...key, disposition: "attached" }),
      sev_5: ev("sev_5", "src_mail", "Re: [org/app] 卡片移到最上方 (PR #227)", 6, { ...key, disposition: "attached" }),
      sev_6: ev("sev_6", "src_github", "feat(desktop): 卡片移到最上方", 0, key),
    });
    const card = await screen.findByRole("article", { name: "审阅 PR #227" });
    await userEvent.click(within(card).getByText("6 条消息 → 这一条"));
    expect(await within(card).findByText("iCloud 邮件 2 条 · 邮件 3 条 · GitHub 1 条")).toBeInTheDocument();
    const [first, second] = within(card).getAllByRole("listitem", { name: "iCloud 邮件" });
    expect(first).toHaveTextContent("最先到，建出这条 · 原文 · 链接");
    expect(second).toHaveTextContent("链接相同（github.com/org/app/pull/227），规则合入这条 · 相似度：键相同，不用算 · 合并于");
    const mails = within(card).getAllByRole("listitem", { name: "邮件" });
    expect(mails[1]).toHaveTextContent("同一件事的后续消息，挂到这条 · 原文 · 链接");
    expect(within(card).getByRole("listitem", { name: "GitHub" })).toHaveTextContent("规则合入这条");
    expect(within(card).queryByRole("button", { name: "撤销合并" }), "规则当场合并的没有折叠组").toBeNull();
    expect(within(card).getAllByRole("button", { name: "拆出来" })).toHaveLength(5);
    expect(first).toHaveAttribute("title", "sev_1"); // 悬停看全值
    noIds();
  });

  it("改前：跨批的两跳串成一组——「评审…」连同它的 4 条（其中 1 条此前按标题与发件人合入它）→ 这条；撤销合并仍发 POST /items/{被折叠的那条}/unmerge", async () => {
    const sweep = (event: string, source: string, item: string, score: number, why: string) => ({ event, source, title: "x", by: "generate", score, why, batch: "mb_1", item, at: at(57) });
    const y = prop("itm_y", { title: "审阅 PR #227", origin: { source: "src_mail_icloud" }, proposal: { low_confidence: false,
      sources: ["sev_y", "sev_a1", "sev_a2", "sev_a3", "sev_a4", "sev_w1"],
      merges: [...["sev_a1", "sev_a2", "sev_a3", "sev_a4"].map((e, i) => sweep(e, i ? "src_mail" : "src_mail_icloud", "itm_x", 0.93, "都是 PR #227 的评审请求")),
        sweep("sev_w1", "src_github", "itm_w", 0.47, "同一个 PR 的平台通知")] } as any });
    const s = mountWith([y], {
      sev_y: ev("sev_y", "src_mail_icloud", "Re: [org/app] 卡片 (PR #227)", 0),
      sev_a1: ev("sev_a1", "src_mail_icloud", "[org/app] 卡片 (PR #227)", 0, { merge_key: "title:[org/app] 卡片 (pr #227)" }),
      sev_a2: ev("sev_a2", "src_mail", "[org/app] 卡片 (PR #227)", 0, { merge_key: "title:[org/app] 卡片 (pr #227)" }),
      sev_a3: ev("sev_a3", "src_mail", "Re: [org/app] 卡片 (PR #227)", 0, { disposition: "attached" }),
      sev_a4: ev("sev_a4", "src_mail", "Re: [org/app] 卡片 (PR #227)", 6, { disposition: "attached" }),
      sev_w1: ev("sev_w1", "src_github", "feat(desktop): 卡片", 0, { link: "https://github.com/org/app/pull/227" }),
    }, {
      "/v1/items/itm_x": { item: { id: "itm_x", phase: "dropped", title: "评审 PR #227", proposal: { low_confidence: false, sources: ["sev_a1", "sev_a2", "sev_a3", "sev_a4"],
        merges: [{ event: "sev_a2", source: "src_mail", title: "x", by: "merge_key", at: at(47) }] } } },
      "/v1/items/itm_w": { item: { id: "itm_w", phase: "dropped", title: "feat(desktop): 卡片", proposal: { low_confidence: false, sources: ["sev_w1"] } } },
    });
    const card = await screen.findByRole("article", { name: "审阅 PR #227" });
    await userEvent.click(within(card).getByText("6 条消息 → 这一条"));
    const x = await within(card).findByRole("listitem", { name: "「评审 PR #227」并进来的几条" });
    expect(x).toHaveTextContent("「评审 PR #227」连同 4 条消息 → 这条");
    expect(x).toHaveTextContent("那批自动归类 · 写作模型判断为同一件事，自动合并（与生成事项和建议相同） · 标题相似度 0.93");
    expect(x).toHaveTextContent("理由：都是 PR #227 的评审请求");
    const inner = within(x).getAllByRole("listitem");
    expect(inner.map((li) => li.getAttribute("aria-label"))).toEqual(["iCloud 邮件", "邮件", "邮件", "邮件"]);
    expect(inner[0]).toHaveTextContent("最先到，建出「评审 PR #227」");
    expect(inner[1]).toHaveTextContent("标题和发件人都相同，规则合入「评审 PR #227」 · 相似度：键相同，不用算 · 合并于");
    expect(inner[2]).toHaveTextContent("同一件事的后续消息，挂到「评审 PR #227」");
    const w = within(card).getByRole("listitem", { name: "「feat(desktop): 卡片」并进来的几条" });
    expect(w).toHaveTextContent("「feat(desktop): 卡片」连同 1 条消息 → 这条");
    expect(w).toHaveTextContent("标题相似度 0.47");
    expect(within(card).getAllByRole("listitem", { name: "iCloud 邮件" })[0]).toHaveTextContent("最先到，建出这条");
    noIds();
    await userEvent.click(within(x).getAllByRole("button", { name: "撤销合并" })[0]);
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/items/itm_x/unmerge")).toMatchObject({ method: "POST", csrf: "tok-1" }));
    expect(await screen.findByText(/已撤销合并，它回到了建议里/)).toBeInTheDocument();
  });

  it("修复后：折叠时被折那条名下的消息全部搬过来——平台上 PR 的 3 条更新随 pr.opened 一起进组，按到达先后列；卡片与最近自动归类的条数都是搬完后的", async () => {
    const sweep = (event: string, source: string, item: string, score: number, why: string) => ({ event, source, title: "x", by: "generate", score, why, batch: "mb_1", item, at: at(57) });
    const ws = ["sev_w1", "sev_w2", "sev_w3", "sev_w4"];
    const y = prop("itm_y", { title: "审阅 PR #227", origin: { source: "src_mail_icloud" }, proposal: { low_confidence: false,
      sources: ["sev_y", "sev_a1", "sev_a2", "sev_a3", "sev_a4", ...ws],
      merges: [...["sev_a1", "sev_a2", "sev_a3", "sev_a4"].map((e, i) => sweep(e, i ? "src_mail" : "src_mail_icloud", "itm_x", 0.93, "都是 PR #227 的评审请求")),
        ...ws.map((e) => sweep(e, "src_github", "itm_w", 0.47, "同一个 PR 的平台通知"))] } as any });
    const gh = { link: "https://github.com/org/app/pull/227" };
    mountWith([y], {
      sev_y: ev("sev_y", "src_mail_icloud", "Re: [org/app] 卡片 (PR #227)", 0),
      sev_a1: ev("sev_a1", "src_mail_icloud", "[org/app] 卡片 (PR #227)", 0),
      sev_a2: ev("sev_a2", "src_mail", "[org/app] 卡片 (PR #227)", 0, { merge_key: "title:[org/app] 卡片 (pr #227)" }),
      sev_a3: ev("sev_a3", "src_mail", "Re: [org/app] 卡片 (PR #227)", 0, { disposition: "attached" }),
      sev_a4: ev("sev_a4", "src_mail", "Re: [org/app] 卡片 (PR #227)", 6, { disposition: "attached" }),
      sev_w1: ev("sev_w1", "src_github", "feat(desktop): 卡片", 0, gh),
      sev_w2: ev("sev_w2", "src_github", "PR #227 已合并", 31, { ...gh, disposition: "attached" }),
      sev_w3: ev("sev_w3", "src_github", "PR #227 有新提交", 31, { ...gh, disposition: "attached" }),
      sev_w4: ev("sev_w4", "src_github", "请你评审 PR #227", 32, { ...gh, disposition: "attached" }),
    }, {
      "/v1/items/itm_x": { item: { id: "itm_x", phase: "dropped", title: "评审 PR #227", proposal: { low_confidence: false, sources: ["sev_a1", "sev_a2", "sev_a3", "sev_a4"] } } },
      "/v1/items/itm_w": { item: { id: "itm_w", phase: "dropped", title: "feat(desktop): 卡片", proposal: { low_confidence: false, sources: ["sev_w1"] } } },
    });
    const card = await screen.findByRole("article", { name: "审阅 PR #227" });
    await userEvent.click(within(card).getByText("9 条消息 → 这一条"));
    expect(await within(card).findByText("iCloud 邮件 2 条 · 邮件 3 条 · GitHub 4 条")).toBeInTheDocument();
    const w = within(card).getByRole("listitem", { name: "「feat(desktop): 卡片」并进来的几条" });
    expect(w).toHaveTextContent("「feat(desktop): 卡片」连同 4 条消息 → 这条");
    const inner = within(w).getAllByRole("listitem");
    expect(inner.map((li) => li.getAttribute("title"))).toEqual(ws);
    expect(inner[0]).toHaveTextContent("feat(desktop): 卡片");
    expect(inner[0]).toHaveTextContent("最先到，建出「feat(desktop): 卡片」 · 原文 · 链接");
    expect(inner[1]).toHaveTextContent("PR #227 已合并");
    expect(inner[1]).toHaveTextContent("同一件事的后续消息，挂到「feat(desktop): 卡片」 · 原文 · 链接");
    expect(inner[3]).toHaveTextContent("请你评审 PR #227");
    expect(within(w).getByRole("button", { name: "撤销合并" })).toBeInTheDocument();
    noIds();
  });

  it("最近自动归类：同一批里折进同一条的写成「2 条建议（5 条消息）→ 「审阅 PR #227」」，逐条可撤销", async () => {
    const activity = { days: 7, pending: 0, model_calls: 1, chars: 2239, batches: [{ batch: "mb_1", at: at(57), size: 6, run: "run_1", chars: 2239, decisions: [
      { item: "itm_x", title: "评审 PR #227", source: "src_mail_icloud", sources: 4, action: "merge", target: "itm_y", target_title: "审阅 PR #227", why: "都是 PR #227 的评审请求", by: "generate" },
      { item: "itm_w", title: "feat(desktop): 卡片", source: "src_github", sources: 1, action: "merge", target: "itm_y", target_title: "审阅 PR #227", why: "同一个 PR", by: "generate" },
      { item: "itm_y", title: "审阅 PR #227", source: "src_mail_icloud", sources: 6, action: "keep", why: "编号不同，不是同一个 PR", by: "generate" }] }] };
    const s = mountWith([], {}, { "/v1/merge/activity?days=7": activity });
    const panel = (await screen.findByText(/最近 7 天自动归类/)).closest("details")!;
    await userEvent.click(within(panel).getByText(/最近 7 天自动归类/));
    const into = within(panel).getByRole("listitem", { name: "合进「审阅 PR #227」" });
    expect(into).toHaveTextContent("2 条建议（5 条消息）→ 「审阅 PR #227」");
    expect(within(into).getByRole("listitem", { name: "评审 PR #227" })).toHaveTextContent("评审 PR #227 · iCloud 邮件 · 4 条消息合成一条建议 · 写作模型判断理由：都是 PR #227 的评审请求");
    expect(within(panel).getAllByRole("listitem", { name: "审阅 PR #227" }).at(-1)).toHaveTextContent("留着 · 写作模型判断");
    noIds();
    await userEvent.click(within(within(into).getByRole("listitem", { name: "feat(desktop): 卡片" })).getByRole("button", { name: "撤销合并" }));
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/items/itm_w/unmerge")).toMatchObject({ method: "POST", csrf: "tok-1" }));
  });
});

// 降噪（主文档 §7.2）：需要拍板的置顶不折叠；代码托管通知默认收起、一键展开、同一对象合组；批量忽略二次确认、带幂等键、可撤销。
describe("建议页降噪：折叠、合组与批量忽略", () => {
  const PR7 = "o/r/pull/7";
  const merged = { event: "sev_m7", type: "pr.merged", at: "2030-01-02T00:00:00Z" };
  const items = [
    prop("gh7", { title: "PR 7 开了", origin: { source: "src_gh" } }),
    prop("ml7", { title: "Re: PR 7 的邮件", origin: { source: "src_mail_qq" } }),
    prop("gh8", { title: "PR 8 开了", origin: { source: "src_gh" } }),
    { ...prop("dec", { title: "要不要接这个 PR 的评审", origin: { source: "src_gh" } }), kind: "decision" } as P,
    prop("urg", { title: "今天 16:00 前回复", origin: { source: "src_mail_qq" }, proposal: { low_confidence: false, urgent: true } as any }),
    prop("m1", { title: "普通邮件一", origin: { source: "src_mail_qq" } }),
    prop("m2", { title: "普通邮件二", origin: { source: "src_mail_qq" } }),
  ].map((p, n) => ({ ...p, created_at: `2030-01-01T0${n}:00:00Z` }));
  const origins = {
    gh7: { type: "pr.opened", object: PR7, ended: merged }, ml7: { type: "mail.received", object: PR7, ended: merged },
    gh8: { type: "pr.opened", object: "o/r/pull/8" }, dec: { type: "pr.opened", object: "o/r/pull/9" },
    urg: { type: "mail.received" }, m1: { type: "mail.received" }, m2: { type: "mail.received" },
  };
  // 替身按 POST /proposals/batch 的契约应答：忽略记批次号，phase=dropped 给 dismissals，undo 按批改回。seed 是已经忽略过的。
  function backend(batch?: (body: any, key: string) => Response, seed: Record<string, { batch?: string; reason?: string; at: string }> = {}) {
    let live = items.filter((i) => !seed[i.id]);
    const dismissed: Record<string, { batch?: string; reason?: string; at: string }> = { ...seed };
    let n = 0;
    const calls: { method: string; path: string; body?: any; key?: string }[] = [];
    const impl = (async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? "GET";
      const body = init.body ? JSON.parse(init.body as string) : undefined;
      calls.push({ method, path: url, body, key: (init.headers as Record<string, string> | undefined)?.["Idempotency-Key"] });
      if (url === "/v1/projects") return json(200, { projects: [] });
      if (url === "/v1/sources") return json(200, { sources: [{ id: "src_gh", name: "GitHub" }, { id: "src_mail_qq", name: "QQ 邮箱" }] });
      if (url === "/v1/items?phase=proposed") return json(200, { items: live, last_rejections: {}, origins });
      if (url === "/v1/items?phase=dropped") return json(200, { items: items.filter((i) => dismissed[i.id]), last_rejections: {}, dismissals: dismissed });
      if (url === "/v1/proposals/batch") {
        const r = batch?.(body, calls.at(-1)!.key!);
        if (r) return r;
        if (body.mode === "undo") {
          const ids = Object.keys(dismissed).filter((id) => dismissed[id].batch === body.batch);
          ids.forEach((id) => delete dismissed[id]);
          live = [...live, ...items.filter((i) => ids.includes(i.id))];
          return json(200, { results: ids.map((id) => ({ id, do: "undo", ok: true, batch: body.batch })) });
        }
        const b = "pb_" + String(++n).padStart(12, "0");
        const ids: string[] = body.mode === "ended" ? body.filter.ids : body.actions.map((a: any) => a.id);
        const ok = ids.filter((id) => body.mode !== "ended" || (origins as any)[id]?.ended);
        live = live.filter((i) => !ok.includes(i.id));
        ok.forEach((id) => (dismissed[id] = { batch: b, reason: "r", at: "2030-01-03T00:00:00Z" }));
        return json(200, { results: ids.map((id) => (ok.includes(id) ? { id, do: "dismiss", ok: true, batch: b } : { id, do: "dismiss", ok: false, code: "conflict", why: "the upstream object is not verifiably merged or closed; left as is", batch: b })) });
      }
      return json(404, { code: "not_found", why: url, next: "x" });
    }) as unknown as typeof fetch;
    return { impl, calls };
  }
  const hosted = async () => (await screen.findByRole("group", { name: "代码托管通知" })) as HTMLDetailsElement;

  it("需要拍板的置顶、不进任何折叠；代码托管通知默认收起，点一下展开；其余按来源分组且展开", async () => {
    mount(backend().impl);
    const pinned = await screen.findByRole("region", { name: "需要你拍板" });
    expect(within(pinned).getByRole("article", { name: "要不要接这个 PR 的评审" })).toBeInTheDocument(); // 属于 PR 也不折叠
    expect(within(pinned).getByRole("article", { name: "今天 16:00 前回复" })).toBeInTheDocument();
    expect(within(pinned).getAllByRole("article")).toHaveLength(2);
    const box = await hosted();
    expect(box.open).toBe(false);
    expect(box).toHaveTextContent("代码托管通知 3 条 · 2 个 PR/Issue · 2 条已结束（已收起）");
    expect(box.closest("details")!.contains(screen.getByRole("article", { name: "要不要接这个 PR 的评审" }))).toBe(false);
    await userEvent.click(within(box).getByText(/已收起/));
    expect(box.open).toBe(true);
    const mail = screen.getByRole("group", { name: "QQ 邮箱" }) as HTMLDetailsElement;
    expect(mail.open).toBe(true);
    expect(within(mail).getAllByRole("article").map((a) => a.getAttribute("aria-label"))).toEqual(["普通邮件一", "普通邮件二"]);
    // 默认能看到的：拍板 2 条 + 其余 2 条；代码托管 3 条收在折叠里
    expect(document.querySelector("main")!.textContent).not.toMatch(/src_/);
  });

  it("同一个 PR 的通知合成一组：组头写对象与已合并时间，组内列出每条的来源与时间，逐条操作照旧", async () => {
    const s = backend();
    mount(s.impl);
    const box = await hosted();
    const g = within(box).getByRole("region", { name: PR7 });
    expect(g).toHaveTextContent(PR7 + " · 2 条");
    expect(g).toHaveTextContent("已合并");
    const rows = within(within(g).getByRole("list", { name: "这一组的来源与时间" })).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      `GitHub · ${new Date("2030-01-01T00:00:00Z").toLocaleString()} · PR 7 开了`,
      `QQ 邮箱 · ${new Date("2030-01-01T01:00:00Z").toLocaleString()} · Re: PR 7 的邮件`,
    ]);
    expect(within(g).getAllByRole("article")).toHaveLength(2);
    const single = within(box).getByRole("region", { name: "o/r/pull/8" });
    expect(within(single).queryByRole("list", { name: "这一组的来源与时间" })).toBeNull();
    for (const n of ["接受", "补到已有事项", "忽略"]) expect(within(within(g).getByRole("article", { name: "PR 7 开了" })).getByRole("button", { name: n })).toBeEnabled();
  });

  it("忽略已合并或关闭的：先写明条数与依据，确认才发 ended，带幂等键；拿不准的不动并说明；回执上按批撤销，也要二次确认", async () => {
    const s = backend();
    mount(s.impl);
    const box = await hosted();
    await userEvent.click(within(box).getByRole("button", { name: "忽略已合并或关闭的 2 条…" }));
    const ask = within(box).getByRole("group", { name: "确认批量忽略" });
    expect(ask).toHaveTextContent("将忽略已合并 2 条、已关闭 0 条，依据事件写进理由；另有 1 条拿不准，不动");
    expect(s.calls.some((c) => c.path === "/v1/proposals/batch")).toBe(false);
    await userEvent.click(within(ask).getByRole("button", { name: "确认忽略 2 条" }));
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/proposals/batch")).toBeTruthy());
    const b = s.calls.find((c) => c.path === "/v1/proposals/batch")!;
    expect(b.body).toEqual({ mode: "ended", filter: { ids: ["gh7", "ml7"] } });
    expect(b.key).toMatch(/^[0-9a-f-]{36}$/);
    expect(await screen.findByRole("status", { name: "批量处理结果" })).toHaveTextContent("已处理 2 条");
    await waitFor(() => expect(screen.queryByRole("article", { name: "PR 7 开了" })).toBeNull());
    const undo = screen.getByRole("status", { name: "批量忽略" });
    expect(undo).toHaveTextContent("已批量忽略 2 条");
    await userEvent.click(within(undo).getByRole("button", { name: "撤销" }));
    expect(within(undo).getByRole("group", { name: "确认批量忽略" })).toHaveTextContent("将把这批 2 条改回建议，回到原来的分组；之后在别处改过的不动");
    expect(s.calls.filter((c) => c.body?.mode === "undo")).toHaveLength(0);
    await userEvent.click(within(undo).getByRole("button", { name: "确认撤销 2 条" }));
    await waitFor(() => expect(s.calls.filter((c) => c.body?.mode === "undo").map((c) => c.body)).toEqual([{ mode: "undo", batch: "pb_000000000001" }]));
    expect(s.calls.find((c) => c.body?.mode === "undo")!.key).toMatch(/^[0-9a-f-]{36}$/);
    expect(s.calls.some((c) => c.path.endsWith("/transition"))).toBe(false);
    expect(await screen.findByRole("status", { name: "批量忽略" })).toHaveTextContent("已改回建议 2 条");
    expect(await screen.findByRole("article", { name: "PR 7 开了" })).toBeInTheDocument();
  });

  it("幂等：一次确认没有确知结果时留在确认里，重发用同一个幂等键", async () => {
    let fail = true;
    const s = backend(() => (fail ? json(500, { code: "internal", why: "boom", next: "retry" }) : (undefined as unknown as Response)));
    mount(s.impl);
    const mail = await screen.findByRole("group", { name: "QQ 邮箱" });
    await userEvent.click(within(mail).getByRole("button", { name: "忽略这一组 2 条…" }));
    expect(within(mail).getByRole("group", { name: "确认批量忽略" })).toHaveTextContent("将忽略「QQ 邮箱」这一组的 2 条；需要你拍板的不在组里，不受影响。");
    await userEvent.click(within(mail).getByRole("button", { name: "确认忽略 2 条" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(within(mail).getByRole("button", { name: "确认忽略 2 条" }));
    await screen.findByRole("status", { name: "批量处理结果" });
    const posts = s.calls.filter((c) => c.path === "/v1/proposals/batch");
    expect(posts).toHaveLength(2);
    expect(posts[1].key).toBe(posts[0].key);
    expect(posts[1].body).toEqual({ mode: "explicit", actions: [{ id: "m1", do: "dismiss", reason: "批量忽略：QQ 邮箱" }, { id: "m2", do: "dismiss", reason: "批量忽略：QQ 邮箱" }] });
    expect(posts[1].body.actions.map((a: any) => a.id)).not.toContain("urg"); // 需要拍板的那条同来源，也不在组里
  });

  it("按类型分组：按来源事件类型的大类，换轴不丢条目", async () => {
    mount(backend().impl);
    await screen.findByRole("group", { name: "QQ 邮箱" });
    await userEvent.selectOptions(screen.getByLabelText("分组"), "type");
    const mail = screen.getByRole("group", { name: "邮件" });
    expect(within(mail).getAllByRole("article")).toHaveLength(2);
  });

  it("已忽略：按批列出（哪一次、理由、时刻），逐条忽略的只列出；按批撤销要二次确认；撤销后回到原来的分组与折叠", async () => {
    const at = "2030-01-03T08:00:00Z";
    const s = backend(undefined, {
      gh7: { batch: "pb_aaaaaaaaaaaa", reason: "upstream o/r/pull/7 ended: pr.merged sev_m7 at 2030-01-02T00:00:00Z", at },
      m1: { batch: "pb_aaaaaaaaaaaa", reason: "批量忽略：QQ 邮箱", at },
      m2: { reason: "不需要", at: "2030-01-01T08:00:00Z" },
    });
    mount(s.impl);
    await hosted();
    expect(screen.queryByRole("article", { name: "PR 7 开了" })).toBeNull();
    await userEvent.selectOptions(screen.getByLabelText("显示"), "ignored");
    const head = `${new Date(at).toLocaleString()} 批量忽略 2 条`;
    const batch = await screen.findByRole("region", { name: head });
    expect(batch).toHaveTextContent("每条理由不同，见下");
    expect(within(batch).getByRole("listitem", { name: "PR 7 开了" })).toHaveTextContent(`GitHub · ${new Date(at).toLocaleString()} · 理由：upstream o/r/pull/7 ended: pr.merged sev_m7`);
    expect(within(batch).getByRole("listitem", { name: "普通邮件一" })).toHaveTextContent("理由：批量忽略：QQ 邮箱");
    const single = screen.getByRole("region", { name: "逐条忽略的 1 条" });
    expect(within(single).getByRole("listitem", { name: "普通邮件二" })).toHaveTextContent("理由：不需要");
    expect(within(single).queryByRole("button")).toBeNull();

    await userEvent.click(within(batch).getByRole("button", { name: "撤销这一批（2 条）…" }));
    expect(s.calls.some((c) => c.body?.mode === "undo")).toBe(false);
    await userEvent.click(within(batch).getByRole("button", { name: "确认撤销 2 条" }));
    await waitFor(() => expect(s.calls.filter((c) => c.body?.mode === "undo").map((c) => c.body)).toEqual([{ mode: "undo", batch: "pb_aaaaaaaaaaaa" }]));
    expect(await screen.findByRole("status", { name: "批量忽略" })).toHaveTextContent("已改回建议 2 条");
    await waitFor(() => expect(screen.queryByRole("region", { name: head })).toBeNull());
    expect(screen.getByRole("region", { name: "逐条忽略的 1 条" })).toBeInTheDocument();

    await userEvent.selectOptions(screen.getByLabelText("显示"), "pending");
    const box = await hosted();
    expect(box.open).toBe(false); // 回到折叠里
    expect(within(within(box).getByRole("region", { name: PR7 })).getByRole("article", { name: "PR 7 开了" })).toBeInTheDocument();
    expect(within(screen.getByRole("group", { name: "QQ 邮箱" })).getByRole("article", { name: "普通邮件一" })).toBeInTheDocument();
  });
});

// 历史清单（主文档 §7.2）：来源前缀命中的迁移清单默认折成一组；规则读 origin.source 的前缀，不认某一个来源名。
describe("建议页折叠历史清单迁移", () => {
  const migrated = Array.from({ length: 91 }, (_, n) => prop("h" + n, { title: "调研 " + n, origin: { source: n < 60 ? "agent:vault-migrate" : "agent:vault-archive" } }));
  const items = [
    ...migrated,
    { ...prop("hd", { title: "迁移来的要不要做", origin: { source: "agent:vault-migrate" } }), kind: "decision" } as P, // 需要拍板的，同来源也置顶
    prop("hh", { title: "迁移来的高优先级", origin: { source: "agent:vault-migrate" }, priority: "high" }),
    prop("d1", { title: "日常建议", origin: { source: "src_mail_qq" } }),
    prop("a1", { title: "别的 agent 的建议", origin: { source: "agent:daily" } }),
    prop("v0", { title: "前缀不符", origin: { source: "agent:vault" } }),
  ];
  const history = async () => (await screen.findByRole("group", { name: "历史清单" })) as HTMLDetailsElement;

  it("91 条迁移清单默认收成一组，写明条数与来源数；日常建议与前缀不符的不进组，需要拍板的置顶且不进组", async () => {
    mount(server(items).impl);
    const pinned = await screen.findByRole("region", { name: "需要你拍板" });
    expect(within(pinned).getAllByRole("article").map((a) => a.getAttribute("aria-label")).sort()).toEqual(["迁移来的要不要做", "迁移来的高优先级"].sort());
    const box = await history();
    expect(box.open).toBe(false);
    expect(box).toHaveTextContent("历史清单 91 条 · 2 个来源（已收起）");
    expect(within(box).getAllByRole("article")).toHaveLength(91);
    expect(box.contains(screen.getByRole("article", { name: "迁移来的要不要做" }))).toBe(false);
    for (const n of ["日常建议", "别的 agent 的建议", "前缀不符"]) expect(box.contains(screen.getByRole("article", { name: n }))).toBe(false);
    expect(screen.getByRole("group", { name: "agent:daily" })).toBeInTheDocument(); // 其余照旧按来源分组且展开
  });

  it("一键展开后按来源分组，仍可逐条接受或忽略（走原接口）", async () => {
    const s = server(items);
    mount(s.impl);
    const box = await history();
    await userEvent.click(within(box).getByText(/已收起/));
    expect(box.open).toBe(true);
    expect(within(within(box).getByRole("region", { name: "agent:vault-migrate" })).getAllByRole("article")).toHaveLength(60);
    expect(within(within(box).getByRole("region", { name: "agent:vault-archive" })).getAllByRole("article")).toHaveLength(31);
    await userEvent.click(within(within(box).getByRole("article", { name: "调研 0" })).getByRole("button", { name: "接受" }));
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/items/h0/accept")).toMatchObject({ method: "POST", csrf: "tok-1" }));
    const card = within(box).getByRole("article", { name: "调研 1" });
    await userEvent.click(within(card).getByRole("button", { name: "忽略" }));
    await userEvent.click(within(card).getByRole("button", { name: "确认忽略" }));
    await waitFor(() => expect(s.calls.find((c) => c.path === "/v1/items/h1/dismiss")!.body).toEqual({}));
    await waitFor(() => expect(within(box).getAllByRole("article")).toHaveLength(89));
    expect(box).toHaveTextContent("历史清单 89 条 · 2 个来源");
  });

  it("没有命中前缀的来源时不出现这一组", async () => {
    mount(server([prop("d1", { title: "日常建议", origin: { source: "src_mail_qq" } })]).impl);
    await screen.findByRole("article", { name: "日常建议" });
    expect(screen.queryByRole("group", { name: "历史清单" })).toBeNull();
  });
});

// 按推荐处理只管看得见的：折叠着的组先展开才算（主文档 §7.2）。代码托管通知与历史清单是同一套折叠，同一规则。
describe("按推荐处理不碰折叠里的建议", () => {
  const rec = (a: string) => ({ proposal: { recommend: a, low_confidence: false } as any });
  const items = [
    prop("v1", { title: "调研一", origin: { source: "agent:vault-migrate" }, ...rec("accept") }),
    prop("v2", { title: "调研二", origin: { source: "agent:vault-migrate" }, ...rec("dismiss") }),
    prop("g1", { title: "PR 通知", origin: { source: "src_gh" }, ...rec("dismiss") }),
    prop("d1", { title: "日常一", origin: { source: "src_mail_qq" }, ...rec("accept") }),
    prop("d2", { title: "日常二", origin: { source: "src_mail_qq" } }),
    { ...prop("dec", { title: "要拍板的", origin: { source: "agent:vault-migrate" }, ...rec("accept") }), kind: "decision" } as P,
  ];
  const origins = { g1: { type: "pr.opened", object: "o/r/pull/1" } };
  const srv = () => {
    const calls: { path: string; body?: any }[] = [];
    const impl = (async (url: string, init: RequestInit = {}) => {
      calls.push({ path: url, body: init.body ? JSON.parse(init.body as string) : undefined });
      if (url === "/v1/projects") return json(200, { projects: [] });
      if (url === "/v1/sources") return json(200, { sources: [] });
      if (url === "/v1/items?phase=proposed") return json(200, { items, last_rejections: {}, origins });
      if (url === "/v1/proposals/batch") return json(200, { results: [] });
      return json(404, { code: "not_found", why: url, next: "x" });
    }) as unknown as typeof fetch;
    return { impl, calls };
  };
  const post = (calls: { path: string; body?: any }[]) => calls.find((c) => c.path === "/v1/proposals/batch")!.body;

  it("两个折叠组都收着时，批量只含置顶与展开的那些：条数与分布不数折叠里的，请求的 ids 也不含", async () => {
    const s = srv();
    mount(s.impl);
    await screen.findByRole("group", { name: "历史清单" });
    await userEvent.click(screen.getByRole("button", { name: "按推荐处理 2 条" })); // dec（置顶）+ d1
    expect(screen.getByText("接受 2 条")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "确认按推荐处理" }));
    await waitFor(() => expect(post(s.calls)).toBeTruthy());
    expect(post(s.calls)).toEqual({ mode: "recommended", filter: { ids: ["dec", "d1"] } });
  });

  it("展开历史清单后它的建议才进批量；再展开代码托管通知也一样；收回去又不算", async () => {
    const s = srv();
    mount(s.impl);
    const history = (await screen.findByRole("group", { name: "历史清单" })) as HTMLDetailsElement;
    const hosted = (await screen.findByRole("group", { name: "代码托管通知" })) as HTMLDetailsElement;
    await userEvent.click(within(history).getByText(/已收起/));
    await userEvent.click(screen.getByRole("button", { name: "按推荐处理 4 条" }));
    expect(screen.getByText("接受 3 条 · 忽略 1 条")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "取消" }));
    await userEvent.click(within(hosted).getByText(/已收起/));
    expect(screen.getByRole("button", { name: "按推荐处理 5 条" })).toBeInTheDocument();
    await userEvent.click(within(history).getByText(/历史清单 2 条/));
    await userEvent.click(screen.getByRole("button", { name: "按推荐处理 3 条" }));
    await userEvent.click(screen.getByRole("button", { name: "确认按推荐处理" }));
    await waitFor(() => expect(post(s.calls)).toBeTruthy());
    expect(post(s.calls)).toEqual({ mode: "recommended", filter: { ids: ["dec", "d1", "g1"] } });
  });
});

// 唯一含推荐的组被收起后，确认不能再发出"没有范围"的请求（服务端缺范围会拒绝，前端同处兜住）。
describe("按推荐处理：确认框打开后收起唯一含推荐的组", () => {
  const items = [
    prop("g1", { title: "PR 通知", origin: { source: "src_gh" }, proposal: { recommend: "dismiss", low_confidence: false } as any }),
    prop("d2", { title: "日常二", origin: { source: "src_mail_qq" } }),
  ];
  const srv = () => {
    const calls: { path: string; body?: any }[] = [];
    const impl = (async (url: string, init: RequestInit = {}) => {
      calls.push({ path: url, body: init.body ? JSON.parse(init.body as string) : undefined });
      if (url === "/v1/projects") return json(200, { projects: [] });
      if (url === "/v1/sources") return json(200, { sources: [] });
      if (url === "/v1/items?phase=proposed") return json(200, { items, last_rejections: {}, origins: { g1: { type: "pr.opened", object: "o/r/pull/1" } } });
      if (url === "/v1/proposals/batch") return json(200, { results: [] });
      return json(404, { code: "not_found", why: url, next: "x" });
    }) as unknown as typeof fetch;
    return { impl, calls };
  };
  const batchCalls = (calls: { path: string; body?: any }[]) => calls.filter((c) => c.path === "/v1/proposals/batch");

  it("展开 → 打开确认 → 收起：确认置灰并写明原因，点了也不发请求；再展开恢复，请求带 ids", async () => {
    const s = srv();
    mount(s.impl);
    const hosted = (await screen.findByRole("group", { name: "代码托管通知" })) as HTMLDetailsElement;
    await userEvent.click(within(hosted).getByText(/已收起/));
    await userEvent.click(screen.getByRole("button", { name: "按推荐处理 1 条" }));
    await userEvent.click(within(hosted).getByText(/代码托管通知/)); // 收起
    expect(hosted.open).toBe(false);
    const ok = screen.getByRole("button", { name: "确认按推荐处理" });
    expect(ok).toBeDisabled();
    expect(screen.getByText(/没有带推荐的.*不能确认/)).toBeInTheDocument();
    await userEvent.click(ok);
    expect(batchCalls(s.calls)).toHaveLength(0);
    await userEvent.click(within(hosted).getByText(/代码托管通知/)); // 再展开
    await userEvent.click(screen.getByRole("button", { name: "确认按推荐处理" }));
    await waitFor(() => expect(batchCalls(s.calls)).toHaveLength(1));
    expect(batchCalls(s.calls)[0].body).toEqual({ mode: "recommended", filter: { ids: ["g1"] } });
  });

  it("折叠组内的建议在任何操作顺序下都不进批量：只要请求发出，ids 就不含 g1", async () => {
    const orders: Array<Array<"open" | "toggle">> = [["toggle", "toggle", "open"], ["open", "toggle", "toggle", "toggle"], ["toggle", "open", "toggle"], ["open"]];
    for (const order of orders) {
      document.body.innerHTML = "";
      const s = srv();
      mount(s.impl);
      const hosted = (await screen.findByRole("group", { name: "代码托管通知" })) as HTMLDetailsElement;
      for (const step of order) {
        if (step === "toggle") await userEvent.click(within(hosted).getByText(/代码托管通知/));
        else {
          const b = screen.queryByRole("button", { name: /^按推荐处理 \d+ 条$/ });
          if (b && !(b as HTMLButtonElement).disabled) await userEvent.click(b);
        }
      }
      const ok = screen.queryByRole("button", { name: "确认按推荐处理" });
      if (ok && !(ok as HTMLButtonElement).disabled) await userEvent.click(ok);
      if (!hosted.open) expect(batchCalls(s.calls), order.join(">")).toHaveLength(0);
      for (const c of batchCalls(s.calls)) expect(hosted.open && c.body.filter.ids, order.join(">")).toEqual(["g1"]);
    }
  });
});
