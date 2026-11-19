import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { setChoice } from "../../app/i18n";
import { feature } from "./index";
import { feature as channels } from "../settings-channels/index";

// 测试边界替身：策略与连接只读，密钥操作按 /settings/ext-keys 的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface Cred { id: string; label?: string; scope: string; revoked_at?: string }

function server(initial: Cred[] = [], deny?: Response, failRevoke?: Response) {
  let creds = initial;
  let n = 0;
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/settings/policy" || url === "/v1/settings/allowlists") return json(200, { version: 1 });
    if (url === "/v1/connections?direction=channel") return json(200, { connections: [] });
    if (url === "/v1/connections/types?direction=channel") return json(200, { types: [] });
    const m = url.match(/^\/v1\/settings\/ext-keys(?:\/(.+))?$/);
    if (!m) return json(404, { code: "not_found", why: "no such route", next: "x" });
    if (deny) return deny.clone();
    const [, id] = m;
    if (method === "GET") return json(200, { credentials: creds });
    if (method === "POST") {
      const c: Cred = { id: "cred_" + ++n, label: body.label, scope: body.scope };
      creds = [...creds, c];
      return json(201, { credential: c, secret: "apa_SECRET_" + n });
    }
    if (failRevoke) return failRevoke.clone();
    const c = creds.find((x) => x.id === id);
    if (!c) return json(404, { code: "not_found", why: "credential not found", next: "list credentials and retry" });
    c.revoked_at = "2026-01-02T00:00:00Z";
    return json(200, { credential: c });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch, at = "/settings/outward-api") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[feature, channels]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const cred = (id: string, scope: string, over: Partial<Cred> = {}): Cred => ({ id, scope, label: "L-" + id, ...over });

beforeEach(() => sessionStorage.clear());

describe("设置 › 系统 › 对外接口 › 导出与转发密钥（flow-bootstrap / flow-authz-matrix）", () => {
  it("ACT-BOOT-04：从设置首页「系统」组可打开，列表只列导出与转发密钥并标出已吊销的；文案分清取数据与收提醒", async () => {
    const s = server([cred("e", "export", { label: "vault" }), cred("f", "forward", { revoked_at: "2026-01-02T00:00:00Z" }), cred("c", "channel.tg", { label: "channel-key" })]);
    mount(s.impl, "/settings/home");
    const entry = within(await screen.findByRole("list", { name: "系统" })).getByRole("link", { name: /对外接口/ });
    await userEvent.click(entry);
    expect(await screen.findByText("vault")).toBeVisible();
    expect(screen.getByText("L-f").closest("li")).toHaveTextContent("已停用");
    expect(screen.queryByText("channel-key")).not.toBeInTheDocument();
    expect(screen.getByText("程序由你自己写，APA 只提供接口和钥匙。")).toBeVisible();
    expect(screen.getByText(/要接收提醒并回答的程序，不在这里/)).toBeVisible();
  });

  it("英文界面有同样的区分", async () => {
    setChoice("en");
    mount(server().impl);
    expect(await screen.findByText(/A program that receives reminders and answers is not here/)).toBeVisible();
    setChoice("zh");
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

  it("离开页面再回来不会显示刚签发的明文", async () => {
    const s = server();
    const first = mount(s.impl);
    await userEvent.click(await screen.findByRole("button", { name: "签发" }));
    expect(await screen.findByText("apa_SECRET_1")).toBeVisible();
    first.unmount();
    mount(s.impl);
    expect(await screen.findByText("cred_1")).toBeVisible();
    expect(screen.queryByText("apa_SECRET_1")).not.toBeInTheDocument();
  });

  it("ACT-AUTH-04：吊销 → DELETE 带 CSRF，随后列表显示已吊销且无吊销按钮", async () => {
    const s = server([cred("a", "export")]);
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
    expect(await screen.findByRole("alert")).toHaveTextContent("Web-session only");
    await userEvent.click(screen.getByRole("button", { name: "签发" }));
    await waitFor(() => expect(s.calls.some((c) => c.method === "POST")).toBe(true));
    expect(document.body.textContent).not.toContain("apa_SECRET");
  });

  it("ACT-AUTH-04：吊销失败（404）展示服务端原因，凭据仍按服务端状态显示", async () => {
    const s = server([cred("a", "export")], undefined, json(404, { code: "not_found", why: "credential not found", next: "list credentials and retry" }));
    mount(s.impl);
    const li = (await screen.findByText("L-a")).closest("li")!;
    await userEvent.click(within(li).getByRole("button", { name: "停用" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("credential not found");
    expect(screen.queryByText(/已停用/)).not.toBeInTheDocument();
  });
});
