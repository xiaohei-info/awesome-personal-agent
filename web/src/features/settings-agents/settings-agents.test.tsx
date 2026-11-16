import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { GRANT_ACTION } from "../../app/ids";
import { policyServer } from "../settings-policy/test-server";
import { feature } from "./index";
import { GRANT_CATS } from "./SettingsAgentsPage";

// 测试边界替身：策略三条路由用 policyServer；声明、钥匙、授权单按 api/settings/{exec,credentials,grants} 的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const L = (zh: string, en: string) => ({ zh, en });
function hermes(rev: number, state: string, access = ["read", "edit", "full"]) {
  const all = { read: [L("APA", "APA"), L("网页", "Web")], edit: [L("APA", "APA"), L("网页", "Web"), L("文件", "Files"), L("终端", "Terminal"), L("委派", "Delegation")] } as Record<string, unknown>;
  all.full = all.edit;
  return {
    harness: "hermes", rev, state,
    declaration: {
      harness: "hermes", name: L("Hermes", "Hermes"),
      headless: {
        command: ["hermes", "chat", "--oneshot"],
        access: Object.fromEntries(access.map((a) => [a, ["-t", "x"]])),
        tools: Object.fromEntries(access.map((a) => [a, all[a]])),
        tool_notes: access.includes("full") ? { full: L("不再逐项问你", "Stops asking you each time") } : {},
      },
    },
  };
}

interface Opts { pending?: any[]; live?: any[]; tokens?: any[]; grants?: any[] }
function server(o: Opts = {}) {
  const s = policyServer();
  const st = { pending: o.pending ?? [], live: o.live ?? [], tokens: o.tokens ?? [], grants: o.grants ?? [] };
  const calls: { method: string; url: string; body?: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    if (method !== "GET") calls.push({ method, url, body });
    if (url === "/v1/settings/declarations") return json(200, { declarations: st.pending });
    if (url === "/v1/settings/commands" && method === "GET") return json(200, { declarations: st.live });
    let m = /^\/v1\/settings\/declarations\/([^/]+)\/(\d+)\/confirm$/.exec(url);
    if (m) {
      const r = st.pending.find((x) => x.harness === m![1] && x.rev === Number(m![2]));
      st.pending = st.pending.filter((x) => x !== r);
      st.live = [{ ...r, state: "active" }];
      return json(200, { harness: m[1], rev: r.rev });
    }
    if (url === "/v1/settings/tokens" && method === "GET") return json(200, { credentials: st.tokens });
    if (url === "/v1/settings/tokens" && method === "POST") return json(201, { credential: { id: "new" }, secret: "apa_dlg_SECRET" });
    m = /^\/v1\/settings\/tokens\/(.+)$/.exec(url);
    if (m && method === "PATCH") {
      st.tokens = st.tokens.map((c) => (c.id === m![1] ? { ...c, harness: body.harness } : c));
      return json(200, { credential: st.tokens.find((c) => c.id === m![1]) });
    }
    if (url === "/v1/settings/grants") return json(200, { grants: st.grants });
    m = /^\/v1\/settings\/agents\/([^/]+)\/grant$/.exec(url);
    if (m) {
      st.grants = st.grants.map((g) => (g.grantee.agent === m![1] && !g.revoked_at ? { ...g, revoked_at: "2030-01-01T00:00:00Z" } : g));
      const g = body.actions.length ? { id: "grt_new", grantee: { agent: m[1], background: body.background }, scope: body.scope, actions: body.actions, created_at: "2030-01-01T00:00:00Z" } : null;
      if (g) st.grants = [...st.grants, g];
      return json(200, { grant: g, revoked: [] });
    }
    if (url.startsWith("/v1/audit/events")) return json(200, { events: [{ seq: 1, type: "item.phase_changed", object: "it_1", at: "2030-01-01T00:00:00Z" }] });
    if (url === "/v1/executors") return json(200, { executors: [] });
    return s.impl(url, init);
  }) as unknown as typeof fetch;
  return { ...s, impl, st, calls };
}

function mount(f: typeof fetch, at = "/settings/agents") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[feature]} fetchImpl={f} />
    </MemoryRouter>,
  );
}
const card = async (name = "Hermes") => within(await screen.findByRole("region", { name }));
const writes = (s: { calls: { method: string; url: string; body?: any }[] }, part: string) => s.calls.filter((c) => c.url.includes(part));

beforeEach(() => sessionStorage.clear());

describe("设置 › Agent", () => {
  it("六类决定合起来正好是可授权动作全集", () => {
    expect(GRANT_CATS.flatMap(([, a]) => a).sort()).toEqual([...GRANT_ACTION].sort());
  });

  it("T13：声明待确认时显示待确认并能允许；允许前不能选档位；钥匙最近由它使用时一键归给它", async () => {
    const s = server({ pending: [hermes(2, "pending")], tokens: [{ id: "k1", kind: "delegate", label: "hermes-default-gateway", seen_harness: "hermes" }] });
    mount(s.impl);
    const c = await card();
    expect(c.getByText(/待确认：允许 APA 在后台拉起它/)).toBeInTheDocument();
    expect(c.getByText(/未接入/)).toBeInTheDocument();
    expect(c.getByText("hermes chat --oneshot")).toBeInTheDocument();
    expect(c.getByRole("button", { name: "可改文件" })).toBeDisabled();
    expect(c.getByText("先允许 APA 在后台拉起它")).toBeInTheDocument();

    await userEvent.click(c.getByRole("button", { name: "归给它" }));
    await waitFor(() => expect(writes(s, "/settings/tokens/")).toEqual([{ method: "PATCH", url: "/v1/settings/tokens/k1", body: { harness: "hermes" } }]));
    expect(await c.findByText(/已接入：可以调用 APA/)).toBeInTheDocument();

    await userEvent.click(c.getByRole("button", { name: "允许" }));
    expect(await c.findByText(/已允许：APA 可以在后台拉起它/)).toBeInTheDocument();
    expect(writes(s, "/confirm")[0].url).toBe("/v1/settings/declarations/hermes/2/confirm");
    expect(c.queryByText(/待确认/)).toBeNull();
  });

  it("T13：档位下列出声明里这一档的工具；没声明的档位置灰", async () => {
    const s = server({ live: [hermes(1, "active", ["read", "edit"])] });
    mount(s.impl);
    const c = await card();
    expect(c.getByTestId("tools-hermes")).toHaveTextContent("APA · 网页");
    await userEvent.click(c.getByRole("button", { name: "可改文件" }));
    expect(c.getByTestId("tools-hermes")).toHaveTextContent("APA · 网页 · 文件 · 终端 · 委派");
    expect(c.getByRole("button", { name: /完全放开/ })).toBeDisabled();
    expect(c.getByRole("button", { name: /完全放开/ })).toHaveTextContent("它没提供");
  });

  it("补充 C：完全放开先预览（含终端与不再逐项问你），确认后才选上；保存只改这个 Agent 的上限", async () => {
    const s = server({ live: [hermes(1, "active")] });
    s.doc.permission_cap = { default: "read", by_harness: { other: "edit" } };
    mount(s.impl);
    const c = await card();
    await userEvent.click(c.getByRole("button", { name: "完全放开" }));
    const pv = c.getByRole("group", { name: "完全放开会给它：" });
    expect(pv).toHaveTextContent("终端");
    expect(pv).toHaveTextContent("不再逐项问你");
    expect(c.getByRole("button", { name: "完全放开" })).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(within(pv).getByRole("button", { name: "确认完全放开" }));
    expect(c.getByRole("button", { name: "完全放开" })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(c.getByRole("button", { name: "保存Agent 后台运行能用什么" }));
    await waitFor(() => expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/access-limits", body: { permission_cap: { default: "read", by_harness: { other: "edit", hermes: "full" } } } }));
  });

  it("以 Agent 本人身份做事：没装唤醒插件时「在对话里」不可选并说明原因；默认单独后台会话", async () => {
    mount(server({ live: [hermes(1, "active")] }).impl);
    const c = within((await card()).getByRole("region", { name: "Agent 自己动手时在哪里做" }));
    expect(c.getByRole("radio", { name: "单独开一个后台会话（不打扰对话）" })).toBeChecked();
    expect(c.getByRole("radio", { name: "在和你的对话里接着做" })).toBeDisabled();
    expect(c.getByText("Agent 没装唤醒插件")).toBeInTheDocument();
  });

  it("以 Agent 本人身份做事：声明支持唤醒时可选「在对话里」，保存只写这个 Agent 的选择，按规则的覆盖原样带回，不重新确认声明", async () => {
    const live = hermes(1, "active");
    (live.declaration as any).main_session = { wake: true };
    const s = server({ live: [live] });
    s.doc.main_session_launch = { by_harness: { other: "headless" }, by_rule: { "p/event_rules/r": "headless" } };
    mount(s.impl);
    const c = within((await card()).getByRole("region", { name: "Agent 自己动手时在哪里做" }));
    expect(c.queryByText("Agent 没装唤醒插件")).toBeNull();
    await userEvent.click(c.getByRole("radio", { name: "在和你的对话里接着做" }));
    await userEvent.click(c.getByRole("button", { name: "保存Agent 自己动手时在哪里做" }));
    await waitFor(() => expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/policy",
      body: { main_session_launch: { by_harness: { other: "headless", hermes: "wake" }, by_rule: { "p/event_rules/r": "headless" } } } }));
    expect(writes(s, "/declarations")).toHaveLength(0);
  });

  it("以 Agent 本人身份做事：旧声明 launch: wake 没选过时显示为「在对话里」", async () => {
    const live = hermes(1, "active");
    (live.declaration as any).main_session = { launch: "wake" };
    mount(server({ live: [live] }).impl);
    const c = within((await card()).getByRole("region", { name: "Agent 自己动手时在哪里做" }));
    expect(c.getByRole("radio", { name: "在和你的对话里接着做" })).toBeChecked();
  });

  it("T14：六类默认都不勾、不签单；勾两类后保存只调一次替换路由，动作是并集映射，默认全部项目、长期、后台可用", async () => {
    const s = server({ live: [hermes(1, "active")] });
    mount(s.impl);
    const c = await card();
    const d = within(c.getByRole("region", { name: "Agent 可以替我决定" }));
    expect(d.getByText("还没允许它替你做决定")).toBeInTheDocument();
    for (const box of d.getAllByRole("checkbox")) expect(box).not.toBeChecked();
    expect(d.queryByRole("button", { name: "保存Agent 可以替我决定" })).toBeNull();
    expect(writes(s, "/grant")).toHaveLength(0);

    await userEvent.click(d.getByLabelText("忽略建议"));
    const d2 = within(c.getByRole("region", { name: "Agent 可以替我决定" }));
    await userEvent.click(d2.getByLabelText("回答普通问题"));
    expect(d2.getByTestId("grant-preview-hermes")).toHaveTextContent("将允许：忽略建议、回答普通问题 · 全部项目 · 一直有效");
    expect(d2.getByLabelText("Agent 后台运行时也可以")).toBeChecked();
    await userEvent.click(d2.getByRole("button", { name: "保存Agent 可以替我决定" }));
    await waitFor(() => expect(writes(s, "/grant")).toHaveLength(1));
    expect(writes(s, "/grant")[0]).toEqual({
      method: "PUT", url: "/v1/settings/agents/hermes/grant",
      body: { actions: ["proposal.dismiss", "decision.answer"], scope: { all: true }, background: true },
    });
  });

  it("T14：已有的单回填勾选；全部取消再保存即只撤销", async () => {
    const g = { id: "grt_1", grantee: { agent: "hermes", background: true }, scope: { all: true }, actions: ["proposal.accept", "proposal.attach"], created_at: "2030-01-01T00:00:00Z" };
    const s = server({ live: [hermes(1, "active")], grants: [g] });
    mount(s.impl);
    const d = within((await card()).getByRole("region", { name: "Agent 可以替我决定" }));
    expect(d.getByLabelText("接受信任发件人带来的建议")).toBeChecked();
    expect(await d.findByText(/用过 1 次/)).toBeInTheDocument();
    await userEvent.click(d.getByLabelText("接受信任发件人带来的建议"));
    expect(d.getByTestId("grant-preview-hermes")).toHaveTextContent("保存后不再允许它替你决定");
    await userEvent.click(d.getByRole("button", { name: "保存Agent 可以替我决定" }));
    await waitFor(() => expect(writes(s, "/grant")).toHaveLength(1));
    expect(writes(s, "/grant")[0].body.actions).toEqual([]);
  });

  it("T15：旧地址 /settings/grants#<id> 落到这一页，旧授权单展开可见", async () => {
    const g = { id: "grt_old", grantee: { credential: "k1" }, scope: { projects: ["acc-home"] }, actions: ["decision.answer"], expires_at: "2099-01-01T00:00:00Z", created_at: "2030-01-01T00:00:00Z" };
    const s = server({ live: [hermes(1, "active")], grants: [g], tokens: [{ id: "k1", kind: "delegate", label: "old key" }] });
    const { container } = mount(s.impl, "/settings/grants#grt_old");
    await card();
    const row = container.querySelector("#grant-grt_old")!;
    expect(row).not.toBeNull();
    expect(row.closest("details")).toHaveAttribute("open");
    expect(row).toHaveTextContent("有效");
  });

  it("T15：旧的后台权限、宿主接入地址也改写到这一页", async () => {
    for (const at of ["/settings/access", "/settings-exec"]) {
      const s = server({ live: [hermes(1, "active")] });
      const v = mount(s.impl, at);
      expect(await screen.findByRole("region", { name: "Hermes" })).toBeInTheDocument();
      v.unmount();
    }
  });
  it("ACT-BOOT-04：卡片里生成的钥匙带上这个 Agent；给执行者的钥匙在「其他」里签，可绑定执行者；明文只显示一次", async () => {
    const s = server({ live: [hermes(1, "active")] });
    mount(s.impl);
    const c = await card();
    await userEvent.click(c.getByRole("button", { name: "生成钥匙" }));
    expect(await screen.findByText("apa_dlg_SECRET")).toBeInTheDocument();
    expect(writes(s, "/settings/tokens")[0]).toEqual({ method: "POST", url: "/v1/settings/tokens", body: { kind: "delegate", harness: "hermes" } });
    const other = within(screen.getByRole("form", { name: "给执行者和其他程序的钥匙" }));
    await userEvent.selectOptions(other.getByLabelText("给谁"), "worker");
    await userEvent.type(other.getByLabelText("备注"), "ci");
    await userEvent.click(other.getByRole("button", { name: "生成钥匙" }));
    await waitFor(() => expect(writes(s, "/settings/tokens")).toHaveLength(2));
    expect(writes(s, "/settings/tokens")[1].body).toEqual({ kind: "worker", label: "ci" });
    await userEvent.click(screen.getByRole("button", { name: "我已保存" }));
    expect(document.body.textContent).not.toContain("apa_dlg_SECRET");
  });
});
