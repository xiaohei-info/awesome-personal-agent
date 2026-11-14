import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/settings/credentials 的契约应答（tokens: observer/worker/delegate；ext-keys: ext，需要 scope）。
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
    if (url === "/v1/settings/executors") return json(200, { executors: [{ id: "exec-1", kind: "external-cli" }] });
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

const section = (name: string) => screen.getByRole("region", { name });
const cred = (id: string, over: Partial<Cred> = {}): Cred => ({ id, kind: "observer", label: "L-" + id, created_at: "2026-01-01T00:00:00Z", ...over });

beforeEach(() => sessionStorage.clear());

describe("凭据设置页（flow-bootstrap / flow-authz-matrix）", () => {
  it("ACT-BOOT-04：分别从 tokens 与 ext-keys 列表，已吊销的标出", async () => {
    const s = server([cred("a"), cred("b", { revoked_at: "2026-01-02T00:00:00Z" }), cred("e", { kind: "ext", scope: "calendar", label: "cal" })]);
    mount(s.impl);
    await screen.findByText("L-a");
    expect(within(section("给 agent 的")).getByText("L-b").closest("li")).toHaveTextContent("已停用");
    expect(within(section("给 agent 的")).getByText("L-a").closest("li")).not.toHaveTextContent("已停用");
    expect(within(section("给扩展程序的")).getByText("cal")).toBeInTheDocument();
    expect(within(section("给 agent 的")).queryByText("cal")).not.toBeInTheDocument();
  });

  it("ACT-BOOT-04：签发令牌 → POST 带 CSRF 与 kind；明文只显示一次，关闭后不再可见、不落存储", async () => {
    const s = server();
    mount(s.impl);
    const tokens = await screen.findByRole("region", { name: "给 agent 的" });
    await userEvent.selectOptions(within(tokens).getByLabelText("给谁"), "worker");
    await userEvent.type(within(tokens).getByLabelText("备注"), "ci");
    const exec = within(tokens).getByLabelText("绑定的执行者");
    await waitFor(() => expect(within(exec).getAllByRole("option").map((o) => o.textContent)).toContain("exec-1"));
    await userEvent.selectOptions(exec, "exec-1");
    await userEvent.click(within(tokens).getByRole("button", { name: "签发" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("apa_SECRET_1");
    const w = s.calls.find((c) => c.method === "POST")!;
    expect(w.path).toBe("/v1/settings/tokens");
    expect(w.csrf).toBe("tok-1");
    expect(w.body).toEqual({ kind: "worker", label: "ci", executor: "exec-1" });
    // 列表里有新凭据，但没有明文
    expect(await within(tokens).findByText("ci")).toBeInTheDocument();
    expect(JSON.stringify(sessionStorage)).not.toContain("SECRET");

    await userEvent.click(within(alert).getByRole("button", { name: "我已保存" }));
    expect(screen.queryByText(/apa_SECRET_1/)).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("apa_SECRET_1");
  });

  it("ACT-BOOT-04：签发 ext 密钥必须有 scope，请求不带 kind 之外的越组字段；用途按类型拼成 scope", async () => {
    const s = server();
    mount(s.impl);
    const ext = await screen.findByRole("region", { name: "给扩展程序的" });
    // 消息渠道要有渠道名
    expect(within(ext).getByRole("button", { name: "签发" })).toBeDisabled();
    await userEvent.type(within(ext).getByLabelText("渠道名"), "chat");
    await userEvent.click(within(ext).getByRole("button", { name: "签发" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("apa_SECRET_1");
    const posts = () => s.calls.filter((c) => c.method === "POST");
    expect(posts()[0].path).toBe("/v1/settings/ext-keys");
    expect(posts()[0].body).toEqual({ scope: "channel.chat" });
    expect(await within(ext).findByText("消息渠道：chat")).toBeInTheDocument();
    await userEvent.selectOptions(within(ext).getByLabelText("用途"), "export");
    await userEvent.click(within(ext).getByRole("button", { name: "签发" }));
    await screen.findByText(/apa_SECRET_2/);
    expect(posts()[1].body).toEqual({ scope: "export" });
    await userEvent.selectOptions(within(ext).getByLabelText("用途"), "forward");
    expect(within(ext).queryByLabelText("名称（可选）")).not.toBeInTheDocument();
    await userEvent.click(within(ext).getByRole("button", { name: "签发" }));
    await screen.findByText(/apa_SECRET_3/);
    expect(posts()[2].body).toEqual({ scope: "forward" });
  });

  it("ACT-BOOT-04：新签发会替换上一条明文提示，不会同时留两个密钥", async () => {
    const s = server();
    mount(s.impl);
    const tokens = await screen.findByRole("region", { name: "给 agent 的" });
    await userEvent.click(within(tokens).getByRole("button", { name: "签发" }));
    await screen.findByText(/apa_SECRET_1/);
    await userEvent.click(within(tokens).getByRole("button", { name: "签发" }));
    await screen.findByText(/apa_SECRET_2/);
    expect(screen.queryByText(/apa_SECRET_1/)).not.toBeInTheDocument();
  });

  it("ACT-AUTH-04：吊销 → DELETE 带 CSRF，随后列表立即显示已吊销且无吊销按钮", async () => {
    const s = server([cred("a")]);
    mount(s.impl);
    const li = (await screen.findByText("L-a")).closest("li")!;
    await userEvent.click(within(li).getByRole("button", { name: "停用" }));
    await within(section("给 agent 的")).findByText(/已停用/);
    const d = s.calls.find((c) => c.method === "DELETE")!;
    expect(d.path).toBe("/v1/settings/tokens/a");
    expect(d.csrf).toBe("tok-1");
    expect(within(section("给 agent 的")).queryByRole("button", { name: "停用" })).not.toBeInTheDocument();
  });

  it("ACT-AUTH-04：非 Web 会话被服务端拒绝（web_only）时，展示服务端原因，不显示成功", async () => {
    const s = server([], json(403, { code: "web_only", why: "this change is Web-session only", next: "do it in the Web UI" }));
    mount(s.impl);
    const tokens = await screen.findByRole("region", { name: "给 agent 的" });
    expect(await screen.findAllByText(/Web-session only/)).not.toHaveLength(0);
    await userEvent.click(within(tokens).getByRole("button", { name: "签发" }));
    await waitFor(() => expect(within(tokens).getByRole("alert")).toHaveTextContent(/Web-session only/));
    expect(s.calls.some((c) => c.method === "POST" && c.path === "/v1/settings/tokens")).toBe(true);
    expect(document.body.textContent).not.toContain("apa_SECRET");
  });

  it("ACT-AUTH-04：吊销失败（404）展示服务端原因，凭据仍按服务端状态显示", async () => {
    const s = server([cred("a")], undefined, json(404, { code: "not_found", why: "credential not found", next: "list credentials and retry" }));
    mount(s.impl);
    const li = (await screen.findByText("L-a")).closest("li")!;
    await userEvent.click(within(li).getByRole("button", { name: "停用" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("credential not found");
    expect(within(section("给 agent 的")).queryByText(/已停用/)).not.toBeInTheDocument();
  });
});
