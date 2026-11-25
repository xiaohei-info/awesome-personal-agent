import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const AG = { type: "agent", principal: "cred_1" };
const CHANGES: any[] = [
  { seq: 9, at: "2030-01-02T00:00:00Z", actor: AG, agent: "agent_1", command: "policy", object: "policy", sections: ["forward_allowlist"], before: { forward_allowlist: {} }, after: { forward_allowlist: { mail: ["a@x"] } }, revert: { ok: true } },
  { seq: 8, at: "2030-01-01T00:00:00Z", actor: AG, agent: "agent_1", command: "instance.run", object: "ins_1", revert: { ok: false, why: "run_started" } },
  { seq: 7, at: "2030-01-01T00:00:00Z", actor: { type: "human", principal: "owner" }, command: "instance.toggle", object: "ins_2", before: { enabled: true }, after: { enabled: false }, revert: { ok: false, why: "superseded" } },
];

function server() {
  const calls: { method: string; url: string; body?: unknown }[] = [];
  let list = CHANGES;
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (method !== "GET") calls.push({ method, url, body: init.body ? JSON.parse(init.body as string) : undefined });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/agents") return json(200, { agents: [{ id: "agent_1", harness: "hermes", name: "日常", enabled: true, rev: 1 }], selected: {}, reason: "" });
    if (url.startsWith("/v1/audit/rule-changes?")) {
      const a = new URL(url, "http://x").searchParams.get("agent");
      return json(200, { changes: list.filter((c) => !a || c.agent === a) });
    }
    if (url === "/v1/audit/rule-changes/9/revert") return (list = list.map((c) => (c.seq === 9 ? { ...c, revert: { ok: false, why: "superseded" } } : c))), json(200, {});
    if (url === "/v1/audit/rule-changes/revert") return json(200, { results: [{ seq: 9, ok: true }, { seq: 8, ok: false, reason: "run_started" }] });
    return json(404, { code: "not_found", why: "no", next: "" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  render(
    <MemoryRouter initialEntries={["/rule-changes"]}>
      <App features={[feature]} fetchImpl={withSession(f)} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("规矩改动（3a 的兜底，设计 §14.3）", () => {
  it("每行有前值、后值与谁改的；撤不回的直接写原因，不给按钮；按次撤回发到这一条", async () => {
    const s = server();
    mount(s.impl);
    const a = await screen.findByRole("article", { name: "信任的发件人 #9" });
    expect(a).toHaveTextContent("日常");
    expect(a).toHaveTextContent('{"forward_allowlist":{"mail":["a@x"]}}');
    const run = screen.getByRole("article", { name: "立即跑安排 #8" });
    expect(run).toHaveTextContent("已拉起的运行撤不回");
    expect(within(run).queryByRole("button")).toBeNull();
    const own = screen.getByRole("article", { name: "安排启停 #7" });
    expect(own).toHaveTextContent("你");
    expect(own).toHaveTextContent("之后又改过");
    await userEvent.click(within(a).getByRole("button", { name: "撤回" }));
    await userEvent.click(within(a).getByRole("button", { name: "确认撤回" }));
    await waitFor(() => expect(s.calls).toEqual([{ method: "POST", url: "/v1/audit/rule-changes/9/revert", body: {} }]));
    expect(await within(await screen.findByRole("article", { name: "信任的发件人 #9" })).findByText(/之后又改过/)).toBeInTheDocument();
  });

  it("按 Agent 批量撤回：先选 Agent，再确认；报告撤回几条、几条撤不回", async () => {
    const s = server();
    mount(s.impl);
    await screen.findByRole("article", { name: "信任的发件人 #9" });
    await userEvent.selectOptions(screen.getByLabelText("谁改的"), "agent_1");
    await waitFor(() => expect(screen.queryByRole("article", { name: "安排启停 #7" })).toBeNull());
    await userEvent.click(screen.getByRole("button", { name: "撤回 日常 的全部改动" }));
    await userEvent.click(screen.getByRole("button", { name: "确认全部撤回" }));
    expect(await screen.findByText("撤回了 1 条，1 条撤不回")).toBeInTheDocument();
    expect(s.calls.at(-1)).toEqual({ method: "POST", url: "/v1/audit/rule-changes/revert", body: { agent: "agent_1" } });
  });
});
