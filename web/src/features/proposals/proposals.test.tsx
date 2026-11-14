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
    expect(note).toHaveTextContent("授权单 grt_bad");
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

  it("ACT-PRH-13：按来源与项目筛选后一次点击，确认后 POST batch {mode:recommended, filter}；只带筛选，不逐条列动作", async () => {
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
    expect(b.body).toEqual({ mode: "recommended", filter: { source: "PRH_A", project: "acc-open" } });
    expect(b.csrf).toBe("tok-1");
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
