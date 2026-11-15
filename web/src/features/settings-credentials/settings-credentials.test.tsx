import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/settings/credentials 的契约应答（ext-keys: ext，需要 scope）。本页只管导出与转发的密钥；
// 给 Agent 与执行者的钥匙见 settings-agents.test，渠道的见 settings-channels.test。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface Cred { id: string; kind: string; label?: string; executor?: string; scope?: string; created_at: string; revoked_at?: string }
const KINDS: Record<string, string[]> = { tokens: ["observer", "worker", "delegate"], "ext-keys": ["ext"] };

function server(initial: Cred[] = [], deny?: Response, failRevoke?: Response) {
  let creds = initial;
  let n = 0;
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/executors") return json(200, { executors: [{ id: "exec-1", kind: "external-cli" }] });
    const m = url.match(/^\/v1\/settings\/(tokens|ext-keys)(?:\/(.+))?$/);
    if (!m) return json(404, { code: "not_found", why: "no such route", next: "x" });
    if (deny) return deny.clone();
    const [, g, id] = m;
    if (method === "GET") return json(200, { credentials: creds.filter((c) => KINDS[g].includes(c.kind)) });
    if (method === "POST") {
      const kind = g === "ext-keys" ? "ext" : body.kind;
      if (!KINDS[g].includes(kind)) return json(400, { code: "invalid_request", why: "kind is not valid", next: "fix" });
      const c: Cred = { id: "cred_" + ++n, kind, label: body.label, executor: body.executor, scope: body.scope, created_at: "2026-01-01T00:00:00Z" };
      creds = [...creds, c];
      return json(201, { credential: c, secret: "apa_SECRET_" + n });
    }
    if (failRevoke) return failRevoke.clone();
    const c = creds.find((x) => x.id === id && KINDS[g].includes(x.kind));
    if (!c) return json(404, { code: "not_found", why: "credential not found", next: "list credentials and retry" });
    c.revoked_at = "2026-01-02T00:00:00Z";
    return json(200, { credential: c });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings-credentials"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const cred = (id: string, over: Partial<Cred> = {}): Cred => ({ id, kind: "observer", label: "L-" + id, created_at: "2026-01-01T00:00:00Z", ...over });

beforeEach(() => sessionStorage.clear());

describe("扩展程序密钥（flow-bootstrap / flow-authz-matrix）", () => {
  it("ACT-BOOT-04：只列导出与转发的密钥，渠道密钥不在这里；已吊销的标出", async () => {
    const ext = (id: string, scope: string, over: Partial<Cred> = {}) => cred(id, { kind: "ext", scope, ...over });
    const s = server([ext("e", "export", { label: "vault" }), ext("f", "forward", { revoked_at: "2026-01-02T00:00:00Z" }), ext("c", "channel.tg", { label: "tg" })]);
    mount(s.impl);
    expect(await screen.findByText("vault")).toBeInTheDocument();
    expect(screen.getByText("L-f").closest("li")).toHaveTextContent("已停用");
    expect(screen.queryByText("tg")).not.toBeInTheDocument();
  });

  it("ACT-BOOT-04：用途按类型拼成 scope；明文只显示一次，关闭后不再可见、不落存储", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.type(await screen.findByLabelText("名称（可选）"), "vault");
    await userEvent.click(screen.getByRole("button", { name: "签发" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("apa_SECRET_1");
    const posts = () => s.calls.filter((c) => c.method === "POST");
    expect(posts()[0]).toMatchObject({ path: "/v1/settings/ext-keys", body: { scope: "export.vault" }, csrf: "tok-1" });
    await userEvent.selectOptions(screen.getByLabelText("用途"), "forward");
    expect(screen.queryByLabelText("名称（可选）")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "签发" }));
    await screen.findByText(/apa_SECRET_2/);
    expect(screen.queryByText(/apa_SECRET_1/)).not.toBeInTheDocument();
    expect(posts()[1].body).toEqual({ scope: "forward" });
    expect(JSON.stringify(sessionStorage)).not.toContain("SECRET");
    await userEvent.click(screen.getByRole("button", { name: "我已保存" }));
    expect(document.body.textContent).not.toContain("apa_SECRET");
  });

  it("ACT-AUTH-04：吊销 → DELETE 带 CSRF，随后列表显示已吊销且无吊销按钮", async () => {
    const s = server([cred("a", { kind: "ext", scope: "export" })]);
    mount(s.impl);
    const li = (await screen.findByText("L-a")).closest("li")!;
    await userEvent.click(within(li).getByRole("button", { name: "停用" }));
    await screen.findByText(/已停用/);
    expect(s.calls.find((c) => c.method === "DELETE")).toMatchObject({ path: "/v1/settings/ext-keys/a", csrf: "tok-1" });
    expect(screen.queryByRole("button", { name: "停用" })).not.toBeInTheDocument();
  });

  it("ACT-AUTH-04：非 Web 会话被服务端拒绝（web_only）时，展示服务端原因，不显示成功", async () => {
    const s = server([], json(403, { code: "web_only", why: "this change is Web-session only", next: "do it in the Web UI" }));
    mount(s.impl);
    expect(await screen.findAllByText(/Web-session only/)).not.toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "签发" }));
    await waitFor(() => expect(s.calls.some((c) => c.method === "POST")).toBe(true));
    expect(document.body.textContent).not.toContain("apa_SECRET");
  });

  it("ACT-AUTH-04：吊销失败（404）展示服务端原因，凭据仍按服务端状态显示", async () => {
    const s = server([cred("a", { kind: "ext", scope: "export" })], undefined, json(404, { code: "not_found", why: "credential not found", next: "list credentials and retry" }));
    mount(s.impl);
    const li = (await screen.findByText("L-a")).closest("li")!;
    await userEvent.click(within(li).getByRole("button", { name: "停用" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("credential not found");
    expect(screen.queryByText(/已停用/)).not.toBeInTheDocument();
  });
});
