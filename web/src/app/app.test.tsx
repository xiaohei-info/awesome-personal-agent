import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { ApiError, createClient } from "./api";
import { collectFeatures, features as registered, type Feature } from "./registry";
import { useSession } from "./session";

// 测试边界替身：按 api/core 的契约（/v1/auth/login、/v1/auth/logout、会话 Cookie + X-CSRF-Token）应答。
function fakeServer() {
  const calls: { method: string; path: string; csrf: string | null }[] = [];
  let sessionValid = false;
  const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  const unauth = () => json(401, { code: "unauthenticated", why: "credential is missing, unknown, revoked or expired", next: "obtain a valid credential in Web" });
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const csrf = (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null;
    calls.push({ method, path: url, csrf });
    if (url === "/v1/auth/login") {
      const pw = JSON.parse(init.body as string).password;
      if (pw !== "correct-horse") return unauth();
      sessionValid = true;
      return json(200, { csrf_token: "tok-1", expires_at: "2030-01-01T00:00:00Z" });
    }
    if (!sessionValid) return unauth();
    if (url === "/v1/auth/logout") {
      if (csrf !== "tok-1") return json(403, { code: "permission_denied", why: "CSRF token is missing or wrong", next: "x" });
      sessionValid = false;
      return json(200, { ok: true });
    }
    return json(200, { projects: [] });
  }) as unknown as typeof fetch;
  return { impl, calls, setValid: () => (sessionValid = true) };
}

const feat = (id: string, title: string, order?: number): Feature => ({ id, title, path: id, order, element: <h2>{title} 页面</h2> });
const FEATURES = [feat("decisions", "决策待办", 1), feat("items", "事项", 2)];

function mount(fetchImpl: typeof fetch, features = FEATURES, at = "/") {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={features} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

async function signIn(pw: string) {
  await userEvent.type(await screen.findByLabelText("口令"), pw);
  await userEvent.click(screen.getByRole("button", { name: "登录" }));
}

beforeEach(() => sessionStorage.clear());

describe("登录与导航框架", () => {
  it("口令错误：显示服务端原因并留在登录页", async () => {
    mount(fakeServer().impl);
    await signIn("wrong-pass");
    expect(await screen.findByRole("alert")).toHaveTextContent("credential is missing");
    expect(screen.queryByRole("navigation")).not.toBeInTheDocument();
  });

  it("登录成功进入外壳：导航来自注册的页面，默认落到第一个页面", async () => {
    mount(fakeServer().impl);
    await signIn("correct-horse");
    const nav = await screen.findByRole("navigation", { name: "主导航" });
    expect(nav).toHaveTextContent("决策待办");
    expect(nav).toHaveTextContent("事项");
    expect(await screen.findByRole("heading", { name: "决策待办 页面" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("link", { name: "事项" }));
    expect(await screen.findByRole("heading", { name: "事项 页面" })).toBeInTheDocument();
    expect(sessionStorage.getItem("apa.csrf")).toBe("tok-1");
  });

  it("刷新后保持会话：csrf 与有效 Cookie 都在则直接进入外壳", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const srv = fakeServer();
    srv.setValid();
    mount(srv.impl);
    expect(await screen.findByRole("navigation", { name: "主导航" })).toBeInTheDocument();
    expect(srv.calls[0]).toMatchObject({ method: "GET", path: "/v1/projects" });
  });

  it("Cookie 已过期：探针 401 回登录页并清掉 csrf", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    mount(fakeServer().impl);
    expect(await screen.findByLabelText("口令")).toBeInTheDocument();
    expect(sessionStorage.getItem("apa.csrf")).toBeNull();
  });

  it("探针网络失败不冒充未登录：显示错误并可重试", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const srv = fakeServer();
    srv.setValid();
    let down = true;
    const impl = ((u: string, i: RequestInit) => (down ? Promise.reject(new TypeError("network down")) : srv.impl(u, i))) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("network down");
    down = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("navigation", { name: "主导航" })).toBeInTheDocument();
  });

  it("退出登录带 CSRF 令牌，成功后回登录页", async () => {
    const srv = fakeServer();
    mount(srv.impl);
    await signIn("correct-horse");
    await userEvent.click(await screen.findByRole("button", { name: "退出登录" }));
    expect(await screen.findByLabelText("口令")).toBeInTheDocument();
    expect(srv.calls.find((c) => c.path === "/v1/auth/logout")?.csrf).toBe("tok-1");
    expect(sessionStorage.getItem("apa.csrf")).toBeNull();
  });

  it("未声明入口的页面各自成为一个入口，排在最后一组；入口数量为其中页面数量之和，为 0 或取不到时不显示", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const srv = fakeServer();
    srv.setValid();
    mount(srv.impl, [
      { ...feat("decisions", "决策待办", 1), section: "decisions", count: async () => 3 },
      { ...feat("proposals", "建议流", 2), section: "proposals", count: async () => 0 },
      { ...feat("items", "事项", 3), count: () => Promise.reject(new Error("down")) },
    ]);
    const nav = await screen.findByRole("navigation", { name: "主导航" });
    expect(within(nav).getAllByRole("heading").map((h) => h.textContent)).toEqual(["处理", "运营"]);
    expect(await within(nav).findByRole("link", { name: "决策待办 3" })).toBeInTheDocument();
    expect(within(nav).getByRole("link", { name: "建议流" })).toBeInTheDocument();
    expect(within(nav).getByRole("link", { name: "事项" })).toBeInTheDocument();
  });

  it("⌘K / Ctrl+K 跳转面板：过滤、上下键选择、回车跳转，Esc 关闭并归还焦点", async () => {
    mount(fakeServer().impl);
    await signIn("correct-horse");
    await screen.findByRole("heading", { name: "决策待办 页面" });
    const jump = screen.getByRole("button", { name: "跳转到页面" });
    jump.focus();

    await userEvent.keyboard("{Control>}k{/Control}");
    const dialog = await screen.findByRole("dialog", { name: "跳转" });
    const input = within(dialog).getByRole("combobox", { name: "跳转到页面" });
    expect(input).toHaveFocus();
    const opts = within(dialog).getAllByRole("option");
    expect(opts.map((o) => o.textContent)).toEqual(["决策待办", "事项"]);
    expect(opts[0]).toHaveAttribute("aria-selected", "true");
    await userEvent.keyboard("{ArrowDown}");
    expect(within(dialog).getAllByRole("option")[1]).toHaveAttribute("aria-selected", "true");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(jump).toHaveFocus();

    await userEvent.click(jump);
    await userEvent.type(screen.getByRole("combobox", { name: "跳转到页面" }), "事项");
    expect(within(screen.getByRole("dialog")).getAllByRole("option")).toHaveLength(1);
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByRole("heading", { name: "事项 页面" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("没有注册页面时给出空状态而不是白屏", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const srv = fakeServer();
    srv.setValid();
    mount(srv.impl, []);
    expect(await screen.findByText("暂无可用页面")).toBeInTheDocument();
  });
});

// 真实注册的页面，只把内容换成标题与当前地址：验证入口、页签与旧地址，不依赖各页面的接口。
function Probe({ title }: { title: string }) {
  const { pathname, search, hash } = useLocation();
  return (
    <main>
      <h2>{title} 页面</h2>
      <p data-testid="at">{pathname + search + hash}</p>
    </main>
  );
}
const REAL = registered.map((f) => ({ ...f, count: undefined, element: <Probe title={f.title} /> }));

async function openAt(at: string) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  const srv = fakeServer();
  srv.setValid();
  mount(srv.impl, REAL, at);
  return screen.findByRole("navigation", { name: "主导航" });
}

describe("导航：入口、页签与旧地址（设计 §4.1）", () => {
  it("三组九个入口，设置在底部；默认落到今天", async () => {
    const nav = await openAt("/");
    expect(within(nav).getAllByRole("heading").map((h) => h.textContent)).toEqual(["处理", "工作", "运营"]);
    expect(within(nav).getAllByRole("link").map((a) => a.textContent)).toEqual([
      "今天", "决策待办", "建议流", "事项", "项目", "agent 动态", "来源", "规则与判断", "健康", "设置",
    ]);
    expect(await screen.findByRole("heading", { name: "今天 页面" })).toBeInTheDocument();
  });

  // 旧地址一个不改：每个原页面仍在原地址，落到所属入口并高亮对应页签。
  const OLD: [string, string, string][] = [
    ["/today", "今天", "今天"], ["/briefs", "今天", "简报"], ["/unread", "今天", "Web 未读"],
    ["/decisions", "决策待办", "决策待办"], ["/proposals", "建议流", "建议流"],
    ["/items", "事项", "事项"], ["/items/itm_1", "事项", "事项"], ["/pack-views", "事项", "能力包视图"],
    ["/projects", "项目", "项目"], ["/materials", "项目", "材料"],
    ["/agent-changes", "agent 动态", "agent 变更"], ["/delegation", "agent 动态", "任务委派"], ["/runs", "agent 动态", "运行记录"],
    ["/sources", "来源", "来源"], ["/rules", "规则与判断", "规则模式"], ["/judgment", "规则与判断", "判断与校准"],
    ["/health", "健康", "健康"], ["/audit", "健康", "审计"],
    ["/settings", "设置", "策略与人格"], ["/settings-credentials", "设置", "凭据"], ["/settings-exec", "设置", "执行者与声明"],
    ["/settings/grants", "设置", "授权单"], ["/settings-security", "设置", "登录安全"], ["/purge", "设置", "物理删除"],
  ];
  it.each(OLD)("%s → 入口「%s」页签「%s」", async (at, entry, tab) => {
    const nav = await openAt(at);
    expect(await screen.findByRole("heading", { name: tab + " 页面" })).toBeInTheDocument();
    expect(within(nav).getByRole("link", { name: entry })).toHaveAttribute("aria-current", "page");
    const tabs = screen.queryByRole("navigation", { name: entry + "页签" });
    if (tabs) expect(within(tabs).getByRole("link", { name: tab })).toHaveAttribute("aria-current", "page");
    else expect(entry).toBe(tab); // 只有一个页面的入口没有页签
  });

  it("简报抽检链接 /judgments/… 改写到判断与校准，保留查询串", async () => {
    await openAt("/judgments/spot-check?judgment=jdg_1");
    expect(await screen.findByRole("heading", { name: "判断与校准 页面" })).toBeInTheDocument();
    expect(screen.getByTestId("at")).toHaveTextContent("/judgment?judgment=jdg_1");
  });

  it("久未推进提醒的锚点保留", async () => {
    await openAt("/today#stale");
    expect(await screen.findByTestId("at")).toHaveTextContent("/today#stale");
  });

  it("动作链接照常可用；动作确认页不高亮任何入口", async () => {
    const nav = await openAt("/act/itm_1?do=done");
    expect(await screen.findByRole("heading", { name: "动作确认 页面" })).toBeInTheDocument();
    expect(within(nav).queryAllByRole("link").filter((a) => a.getAttribute("aria-current"))).toHaveLength(0);
  });

  it("窄屏：顶栏只有四个主路径和「更多」；抽屉列出其余入口，Esc 关闭并归还焦点，点链接跳转后关闭", async () => {
    const mm = window.matchMedia;
    window.matchMedia = ((q: string) => ({ matches: true, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
    try {
      const nav = await openAt("/today");
      expect(within(nav).getAllByRole("link").map((a) => a.textContent)).toEqual(["今天", "决策", "建议", "事项"]);
      const more = within(nav).getByRole("button", { name: "更多" });
      await userEvent.click(more);
      const sheet = screen.getByRole("dialog", { name: "全部页面" });
      expect(within(sheet).getAllByRole("link").map((a) => a.textContent)).toEqual(["项目", "agent 动态", "来源", "规则与判断", "健康", "设置"]);
      expect(within(sheet).getAllByRole("link")[0]).toHaveFocus();
      await userEvent.keyboard("{Escape}");
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(more).toHaveFocus();
      await userEvent.click(more);
      await userEvent.click(within(screen.getByRole("dialog")).getByRole("link", { name: "来源" }));
      expect(await screen.findByRole("heading", { name: "来源 页面" })).toBeInTheDocument();
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    } finally {
      window.matchMedia = mm;
    }
  });
});

describe("类型化 API 客户端", () => {
  it("写请求带 X-CSRF-Token，读请求不带", async () => {
    const srv = fakeServer();
    srv.setValid();
    const c = createClient({ fetch: srv.impl, getCsrf: () => "tok-1", onUnauthenticated: () => {} });
    await c.get("/items");
    await c.post("/items", { title: "x" });
    expect(srv.calls.map((x) => x.csrf)).toEqual([null, "tok-1"]);
  });

  it("会话中途失效（401）通知外壳；错误体保留 code/why/next", async () => {
    let dropped = 0;
    const c = createClient({ fetch: fakeServer().impl, getCsrf: () => null, onUnauthenticated: () => dropped++ });
    const err = (await c.get("/items").catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401, code: "unauthenticated" });
    expect(err.body.next).toContain("Web");
    expect(dropped).toBe(1);
  });

  it("登录口令错误的 401 不触发会话失效回调", async () => {
    let dropped = 0;
    const c = createClient({ fetch: fakeServer().impl, getCsrf: () => null, onUnauthenticated: () => dropped++ });
    await expect(c.login("bad")).rejects.toMatchObject({ status: 401 });
    expect(dropped).toBe(0);
  });

  it("非 JSON 错误响应（反向代理错误页）落到 internal 兜底", async () => {
    const impl = (async () => new Response("<html>bad gateway</html>", { status: 502 })) as unknown as typeof fetch;
    const c = createClient({ fetch: impl, getCsrf: () => null, onUnauthenticated: () => {} });
    await expect(c.get("/items")).rejects.toMatchObject({ status: 502, code: "internal" });
  });

  it("useSession 在 Provider 外使用直接报错", () => {
    function Bad() {
      useSession();
      return null;
    }
    expect(() => render(<Bad />)).toThrow("outside SessionProvider");
  });
});

describe("页面注册", () => {
  it("按 order、再按 id 排序，忽略没有 feature 导出的模块", () => {
    const got = collectFeatures({ a: { feature: feat("b", "B") }, b: { feature: feat("a", "A") }, c: {}, d: { feature: feat("z", "Z", 1) } });
    expect(got.map((f) => f.id)).toEqual(["z", "a", "b"]);
  });

  it("重复 id 在启动时报错", () => {
    expect(() => collectFeatures({ a: { feature: feat("x", "X") }, b: { feature: feat("x", "X2") } })).toThrow("duplicate feature id: x");
  });
});
