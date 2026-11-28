import { withSession } from "../../app/test-session";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/delegation（GET /executors、DELETE /executors/{id}、GET /dispatches）、api/items（GET /items）、
// api/why（GET /audit/events）的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function server(evidence: object = { pr: "https://x/pull/42" }) {
  let execs = [
    { id: "hermes.paseo.mbp.quick", kind: "remote-agent", host: "mbp", harness: "hermes", label: "Paseo · quick @ mbp" },
    { id: "hermes.delegate_task.apa", kind: "native-subagent", host: "apa", harness: "hermes", label: "Hermes 子 agent @ apa" },
    { id: "auto-runner", kind: "external-cli", harness: "paseo", label: "自动化", startable: true, overrides: {} as Record<string, string>,
      overridable: { model: { label: { zh: "模型", en: "Model" }, choices: ["provider/model-a", "provider/model-b"] } } },
  ];
  const puts: unknown[] = [];
  const items = [
    { id: "c1", phase: "open", title: "修登录超时", assignee: "hermes.paseo.mbp.quick" },
    { id: "c2", phase: "done", title: "写周报", assignee: "hermes.paseo.mbp.quick" },
    { id: "c3", phase: "open", title: "问律师", assignee: "lawyer" },
  ];
  const events = [{ seq: 7, object: "c2", at: "2030-01-01T00:00:00Z", payload: { report: { outcome: "done", summary: "周报已写", evidence } } }];
  const dispatches = [
    { id: "dsp_1", item: "c1", executor: "hermes.paseo.mbp.quick", session: "agt_1", state: "ended", resumable: true },
    { id: "dsp_0", item: "c2", executor: "hermes.paseo.mbp.quick", session: "agt_0", state: "lost", resumable: false },
  ];
  const calls: { method: string; path: string }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    calls.push({ method, path: url });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/executors" && method === "GET") return json(200, { executors: execs });
    if (url === "/v1/items?kind=delegation") return json(200, { items });
    if (url === "/v1/dispatches?resumable=true&limit=200") return json(200, { dispatches });
    if (url.startsWith("/v1/audit/events?type=delegation.reported&after=")) return json(200, { events });
    const ov = url.match(/^\/v1\/executors\/(.+)\/overrides$/);
    if (ov && method === "PUT") {
      const body = JSON.parse(String(init.body));
      puts.push(body);
      execs = execs.map((e) => (e.id === decodeURIComponent(ov[1]) ? { ...e, overrides: body.values } : e));
      return json(200, {});
    }
    const rm = url.match(/^\/v1\/executors\/(.+)$/);
    if (rm && method === "DELETE") {
      execs = execs.filter((e) => e.id !== decodeURIComponent(rm[1]));
      return json(200, { id: rm[1] });
    }
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return { impl, calls, puts };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/executors"]}>
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("谁在替你干活（§6.17）", () => {
  it("每个执行者一张卡：手上的事、最近交回、可续接的会话；未登记的也列出", async () => {
    mount(server().impl);
    const paseo = await screen.findByRole("article", { name: "Paseo · quick @ mbp" });
    expect(within(paseo).getByText("修登录超时")).toBeInTheDocument();
    expect(within(paseo).queryByText("写周报")).not.toBeInTheDocument(); // 已完成的不算手上的事
    expect(within(paseo).getByText("周报已写")).toBeInTheDocument();
    expect(within(paseo).getByText("agt_1")).toBeInTheDocument();
    expect(within(paseo).queryByText("agt_0")).not.toBeInTheDocument(); // 失效的会话不列
    expect(within(paseo).queryByRole("button", { name: "移除" })).not.toBeInTheDocument(); // 手上有事不能移除
    const lawyer = screen.getByRole("article", { name: "lawyer" });
    expect(within(lawyer).getByText("问律师")).toBeInTheDocument();
    expect(within(lawyer).getByText(/未登记/)).toBeInTheDocument();
  });

  it("APA 能直接派活的执行者标出来，本人可给它定默认模型 → PUT /executors/{id}/overrides，只能选声明列出的值", async () => {
    const s = server();
    mount(s.impl);
    const auto = await screen.findByRole("article", { name: "自动化" });
    expect(within(auto).getByText("可由 APA 直接派活")).toBeInTheDocument();
    const pick = within(auto).getByRole("combobox", { name: /模型/ });
    expect(within(pick).getAllByRole("option").map((o) => o.textContent)).toEqual(["沿用自带的默认", "provider/model-a", "provider/model-b"]);
    await userEvent.selectOptions(pick, "provider/model-a");
    expect(s.puts).toEqual([{ values: { model: "provider/model-a" } }]);
    expect(within(await screen.findByRole("article", { name: "自动化" })).getByRole("combobox", { name: /模型/ })).toHaveValue("provider/model-a");
  });

  it("回执带多个链接（link 与 links 数组并存）：页面照常渲染，每个链接都列出", async () => {
    mount(server({ pr: "https://x/pull/1", link: "https://x/a", links: ["https://x/a", "https://x/b"] }).impl);
    const paseo = await screen.findByRole("article", { name: "Paseo · quick @ mbp" });
    expect(within(paseo).getAllByRole("link", { name: "link" }).map((a) => a.getAttribute("href"))).toEqual(["https://x/a", "https://x/b"]);
    expect(within(paseo).getByRole("link", { name: "pr" })).toHaveAttribute("href", "https://x/pull/1");
  });

  it("手上没事的名录条目可以移除 → DELETE /executors/{id}", async () => {
    const s = server();
    mount(s.impl);
    const idle = await screen.findByRole("article", { name: "Hermes 子 agent @ apa" });
    await userEvent.click(within(idle).getByRole("button", { name: "移除" }));
    expect(await screen.findByRole("article", { name: "Paseo · quick @ mbp" })).toBeInTheDocument();
    expect(screen.queryByRole("article", { name: "Hermes 子 agent @ apa" })).not.toBeInTheDocument();
    expect(s.calls.some((c) => c.method === "DELETE" && c.path === "/v1/executors/hermes.delegate_task.apa")).toBe(true);
  });
});
