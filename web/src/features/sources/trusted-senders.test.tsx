import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 GET /sources 的 sender、GET /settings/allowlists、self-sender 与
// PUT /settings/sources/{id}/trusted-senders 的契约应答（需确认来源只接受空名单）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const GH = { id: "src_gh", name: "GitHub", type: "pull", enabled: true, sender: { trust: "verified", hint: { zh: "GitHub 用户名", en: "GitHub username" }, self: "identity" } };
const MAIL = { id: "src_mail", name: "gmail 邮件", type: "script", enabled: true, sender: { trust: "unverified" } };

function backend(allow: Record<string, string[]>, self: string | null = "octocat") {
  const puts: { path: string; body: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const u = new URL(url, "http://x");
    if (u.pathname === "/v1/sources") return json(200, { sources: [GH, MAIL] });
    if (u.pathname === "/v1/health") return json(200, { signals: [] });
    if (u.pathname === "/v1/settings/allowlists") return json(200, { version: 1, forward_allowlist: allow, channel_answerers: {} });
    if (u.pathname.endsWith("/self-sender")) return self ? json(200, { sender: self }) : json(409, { code: "conflict", why: "no identity", next: "type it" });
    const ts = u.pathname.match(/^\/v1\/settings\/sources\/([^/]+)\/trusted-senders$/);
    if (ts && method === "PUT") {
      const body = JSON.parse(init.body as string);
      puts.push({ path: u.pathname, body });
      allow[ts[1]] = body.senders;
      return json(200, { version: 2, senders: body.senders });
    }
    if (u.pathname.endsWith("/events")) return json(200, { events: [] });
    if (u.pathname.endsWith("/rejects")) return json(200, { rejects: [] });
    if (u.pathname.endsWith("/ingests")) return json(200, { ingests: [] });
    return json(200, {});
  }) as unknown as typeof fetch;
  return { impl, puts };
}

function mount(f: typeof fetch, at: string) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[feature]} fetchImpl={f} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("来源 › 信任的发件人", () => {
  it("T16 可信来源：从未设置时预填你自己但不保存；点保存才写这个来源的名单", async () => {
    const b = backend({});
    mount(b.impl, "/sources?open=src_gh");
    const sec = within(await screen.findByRole("region", { name: "信任的发件人" }));
    expect(await sec.findByText("已填上你自己，还没保存")).toBeInTheDocument();
    expect(sec.getByRole("button", { name: "去掉 octocat" })).toBeInTheDocument();
    expect(b.puts).toHaveLength(0);
    await userEvent.click(sec.getByRole("button", { name: "保存" }));
    expect(await sec.findByRole("status")).toHaveTextContent("已保存");
    expect(b.puts).toEqual([{ path: "/v1/settings/sources/src_gh/trusted-senders", body: { senders: ["octocat"] } }]);
  });

  it("推不出本人时留空并按声明提示该填什么", async () => {
    mount(backend({}, null).impl, "/sources?open=src_gh");
    const sec = within(await screen.findByRole("region", { name: "信任的发件人" }));
    expect(await sec.findByText("填你自己的GitHub 用户名")).toBeInTheDocument();
    expect(sec.getByRole("textbox", { name: "信任的发件人" })).toHaveAttribute("placeholder", "GitHub 用户名");
  });

  it("T16 需确认来源：没有输入框，只有一句说明；以前填的在高级里标不生效，可以清掉", async () => {
    const b = backend({ src_mail: ["boss@example.com", "me@example.com"] });
    mount(b.impl, "/sources?open=src_mail");
    const sec = within(await screen.findByRole("region", { name: "信任的发件人" }));
    expect(sec.getByText("核实不了发件人：后台不照它办事，自称是你也只给你看")).toBeInTheDocument();
    expect(sec.queryByRole("textbox")).toBeNull();
    expect(sec.getByText(/以前填的 2 个发件人不会生效/)).toBeInTheDocument();
    await userEvent.click(sec.getByText("高级"));
    await userEvent.click(sec.getByRole("button", { name: "清掉" }));
    expect(b.puts).toEqual([{ path: "/v1/settings/sources/src_mail/trusted-senders", body: { senders: [] } }]);
    expect(sec.queryByText(/不会生效/)).toBeNull();
  });

  it("从挂靠消息过来时带上要加的人，不再预填", async () => {
    const b = backend({ src_gh: ["octocat"] });
    mount(b.impl, "/sources?open=src_gh&add=alice");
    const sec = within(await screen.findByRole("region", { name: "信任的发件人" }));
    expect(await sec.findByRole("button", { name: "去掉 alice" })).toBeInTheDocument();
    expect(sec.getByRole("button", { name: "去掉 octocat" })).toBeInTheDocument();
    expect(sec.queryByText("已填上你自己，还没保存")).toBeNull();
  });
});
