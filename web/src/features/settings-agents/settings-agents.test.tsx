import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { policyServer } from "../settings-policy/test-server";
import { feature } from "./index";

const PAGE = "/settings/agents/legacy:hermes";

// 测试边界替身：策略三条路由用 policyServer；声明、钥匙、档位按 api/settings/{exec,credentials,policy} 的契约应答。
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

interface Opts { pending?: any[]; live?: any[]; tokens?: any[]; cards?: any[] }
function server(o: Opts = {}) {
  const s = policyServer();
  const st = { pending: o.pending ?? [], live: o.live ?? [], tokens: o.tokens ?? [], cards: o.cards ?? [] };
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
    if (url === "/v1/agents") return json(200, { agents: st.cards, selected: {}, reason: "" });
    m = /^\/v1\/settings\/agents\/([^/]+)\/tier$/.exec(url);
    if (m) {
      st.cards = st.cards.map((c) => (c.id === m![1] ? { ...c, tier: body.tier, tier_projects: body.projects } : c));
      return json(200, { version: 2 });
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
// 一个 Agent 一页（界面设计 §4）：等页头出现后在主区里找。
const card = async (name = "Hermes") => (await screen.findByRole("heading", { level: 1, name }), within(screen.getByRole("main")));
const writes = (s: { calls: { method: string; url: string; body?: any }[] }, part: string) => s.calls.filter((c) => c.url.includes(part));

beforeEach(() => sessionStorage.clear());

describe("设置 › Agent", () => {
  it("T13：声明待确认时显示待确认并能允许；允许前不能选档位；钥匙最近由它使用时一键归给它", async () => {
    const s = server({ pending: [hermes(2, "pending")], tokens: [{ id: "k1", kind: "delegate", label: "hermes-default-gateway", seen_harness: "hermes" }] });
    mount(s.impl, PAGE);
    const c = await card();
    const connect = within(c.getByRole("region", { name: "接入" }));
    expect(connect.getByText("还差 2 步就能用")).toBeInTheDocument();
    expect(connect.getByText("允许 APA 在后台启动它")).toBeInTheDocument();
    expect(connect.getByText("给它一把钥匙")).toBeInTheDocument();
    await userEvent.click(connect.getByRole("button", { name: "查看并允许" }));
    expect(c.getByText("hermes chat --oneshot")).toBeInTheDocument();
    expect(c.getByRole("button", { name: "可改文件" })).toBeDisabled();
    expect(c.getAllByText("先允许 APA 在后台启动它").length).toBeGreaterThan(0);

    await userEvent.click(c.getByRole("button", { name: "归给它" }));
    await waitFor(() => expect(writes(s, "/settings/tokens/")).toEqual([{ method: "PATCH", url: "/v1/settings/tokens/k1", body: { harness: "hermes" } }]));
    expect(await c.findByText(/已有钥匙，它能找 APA/)).toBeInTheDocument();

    await userEvent.click(c.getByRole("button", { name: "允许" }));
    expect(await c.findByText(/APA 可以在后台启动它/)).toBeInTheDocument();
    expect(writes(s, "/confirm")[0].url).toBe("/v1/settings/declarations/hermes/2/confirm");
    expect(c.getByText("已接好，可以用了")).toBeInTheDocument();
  });

  it("T13：档位下列出声明里这一档的工具；没声明的档位置灰", async () => {
    const s = server({ live: [hermes(1, "active", ["read", "edit"])] });
    mount(s.impl, PAGE);
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
    mount(s.impl, PAGE);
    const c = await card();
    await userEvent.click(c.getByRole("button", { name: "完全放开" }));
    const pv = c.getByRole("group", { name: "完全放开会给它：" });
    expect(pv).toHaveTextContent("终端");
    expect(pv).toHaveTextContent("不再逐项问你");
    expect(c.getByRole("button", { name: "完全放开" })).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(within(pv).getByRole("button", { name: "确认完全放开" }));
    expect(c.getByRole("button", { name: "完全放开" })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(c.getByRole("button", { name: "保存后台权限" }));
    await waitFor(() => expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/access-limits", body: { permission_cap: { default: "read", by_harness: { other: "edit", hermes: "full" } } } }));
  });

  it("以 Agent 本人身份做事：声明不支持唤醒时「在对话里」不可选并说明；默认单独在后台做", async () => {
    mount(server({ live: [hermes(1, "active")] }).impl, PAGE);
    const c = within((await card()).getByRole("region", { name: "在哪里做" }));
    expect(c.getByRole("radio", { name: "单独在后台做（不打扰对话）" })).toBeChecked();
    expect(c.getByRole("radio", { name: "在和你的对话里接着做" })).toBeDisabled();
    expect(c.getByText("Hermes 不支持在对话里接着做")).toBeInTheDocument();
    expect(c.queryByText("怎么打开")).toBeNull();
  });

  it("以 Agent 本人身份做事：唤醒还缺什么用产品语言说，怎么打开收在折叠里", async () => {
    const live = hermes(1, "active");
    (live.declaration as any).main_session = { wake_missing: [
      { ...L("Hermes 还没允许插件在对话里开始一轮", "not allowed"), how: L("在 config.yaml 里加 allow_gateway_injection: true", "add it") },
      { ...L("还没指定要唤醒哪个对话", "no chat"), how: L("设 APA_WAKE_SESSION_KEY", "set it") },
    ] };
    mount(server({ live: [live] }).impl, PAGE);
    const c = within((await card()).getByRole("region", { name: "在哪里做" }));
    expect(c.getByRole("radio", { name: "在和你的对话里接着做" })).toBeDisabled();
    const line = c.getByText("Hermes 还没打开唤醒：Hermes 还没允许插件在对话里开始一轮");
    expect(line.textContent).not.toMatch(/APA_|allow_gateway/);
    const how = c.getByText("怎么打开").closest("details")!;
    expect(how).not.toHaveAttribute("open");
    expect(how).toHaveTextContent("allow_gateway_injection: true");
    expect(how).toHaveTextContent("APA_WAKE_SESSION_KEY");
  });

  it("待确认的声明改了能不能在对话里接着做时，卡片上说明这次变化", async () => {
    const live = hermes(1, "active");
    (live.declaration as any).main_session = { wake: true };
    const pending = hermes(2, "pending");
    (pending.declaration as any).main_session = { wake_missing: [{ ...L("还没指定要唤醒哪个对话", "no chat"), how: L("设", "set") }] };
    mount(server({ live: [live], pending: [pending] }).impl, PAGE);
    const c = await card();
    await userEvent.click(c.getByRole("button", { name: "查看并允许" }));
    expect(c.getByText("这次变化：不能在对话里接着做了（还没指定要唤醒哪个对话）")).toBeInTheDocument();
  });

  it("以 Agent 本人身份做事：声明支持唤醒时可选「在对话里」，保存只写这个 Agent 的选择，按规则的覆盖原样带回，不重新确认声明", async () => {
    const live = hermes(1, "active");
    (live.declaration as any).main_session = { wake: true };
    const s = server({ live: [live] });
    s.doc.main_session_launch = { by_harness: { other: "headless" }, by_rule: { "p/event_rules/r": "headless" } };
    mount(s.impl, PAGE);
    const c = within((await card()).getByRole("region", { name: "在哪里做" }));
    expect(c.queryByText(/不支持在对话里接着做|还没打开唤醒/)).toBeNull();
    // 单选、不扩大权限：选了就存（界面设计 §6.1），就地写「已保存」。
    await userEvent.click(c.getByRole("radio", { name: "在和你的对话里接着做" }));
    expect(await c.findByRole("status")).toHaveTextContent("已保存");
    await waitFor(() => expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/policy",
      body: { main_session_launch: { by_harness: { other: "headless", hermes: "wake" }, by_rule: { "p/event_rules/r": "headless" } } } }));
    expect(writes(s, "/declarations")).toHaveLength(0);
  });

  it("以 Agent 本人身份做事：旧声明 launch: wake 没选过时显示为「在对话里」", async () => {
    const live = hermes(1, "active");
    (live.declaration as any).main_session = { launch: "wake" };
    mount(server({ live: [live] }).impl, PAGE);
    const c = within((await card()).getByRole("region", { name: "在哪里做" }));
    expect(c.getByRole("radio", { name: "在和你的对话里接着做" })).toBeChecked();
  });

  it("档位只读一行并链到「我来拍板」，页上不再有档位卡；「它做过什么」直达按这个 Agent 筛好的记录", async () => {
    const card1 = { id: "agent_1", harness: "hermes", name: "日常", enabled: true, rev: 1, tier: "decide", tier_projects: [] };
    mount(server({ live: [hermes(1, "active")], cards: [card1] }).impl, "/settings/agents/agent_1");
    const c = await card("日常");
    expect(c.queryByRole("region", { name: /能做到哪一档/ })).toBeNull();
    expect(c.getByText("能替你做到：也替我拍板")).toBeInTheDocument();
    expect(c.getByRole("link", { name: "在「我来拍板」改" })).toHaveAttribute("href", "/settings#tier-agent_1");
    const rec = within(c.getByRole("region", { name: "它做过什么" }));
    expect(rec.getByRole("link", { name: /Agent 改动/ })).toHaveAttribute("href", "/agent-changes?agent=agent_1");
    expect(rec.getByRole("link", { name: /规矩改动/ })).toHaveAttribute("href", "/rule-changes?agent=agent_1");
  });

  it("T15：旧地址 /settings/grants 落到 Agent 列表", async () => {
    const s = server({ live: [hermes(1, "active")] });
    mount(s.impl, "/settings/grants");
    expect(await screen.findByRole("listitem", { name: "Hermes" })).toBeInTheDocument();
  });

  it("T15：旧的后台权限、宿主接入地址也改写到这一页", async () => {
    for (const at of ["/settings/access", "/settings-exec"]) {
      const s = server({ live: [hermes(1, "active")] });
      const v = mount(s.impl, at);
      expect(await screen.findByRole("listitem", { name: "Hermes" })).toBeInTheDocument();
      v.unmount();
    }
  });
  it("ACT-BOOT-04：卡片里生成的钥匙带上这个 Agent；给执行者的钥匙在「很少用到」里签，可绑定执行者；明文只显示一次", async () => {
    const s = server({ live: [hermes(1, "active")] });
    const v = mount(s.impl, PAGE);
    const c = await card();
    // 接入清单里就地生成：明文就在这一行，不放到页面顶部（界面设计 §6.2）。
    await userEvent.click(within(c.getByRole("region", { name: "接入" })).getByRole("button", { name: "生成钥匙" }));
    expect(await within(c.getByRole("region", { name: "接入" })).findByText("apa_dlg_SECRET")).toBeInTheDocument();
    expect(writes(s, "/settings/tokens")[0]).toEqual({ method: "POST", url: "/v1/settings/tokens", body: { kind: "delegate", harness: "hermes" } });
    v.unmount();
    mount(s.impl);
    await screen.findByRole("listitem", { name: "Hermes" });
    const other = within(screen.getByRole("form", { name: "给脚本和执行者的钥匙" }));
    expect(other.getByRole("radio", { name: /只读的程序（脚本、看板）/ })).toBeChecked();
    expect(other.getByText("能看事项和材料、提一条等你确认的新事项；不能改事项")).toBeInTheDocument();
    expect(other.queryByLabelText("交回给哪个执行者")).toBeNull();
    await userEvent.click(other.getByRole("radio", { name: /执行者交回结果/ }));
    expect(other.getByText("能更新派给它的事、提问、交材料；不能替你决定")).toBeInTheDocument();
    expect(other.getByText("交回给哪个执行者")).toBeInTheDocument();
    await userEvent.type(other.getByLabelText("备注"), "ci");
    await userEvent.click(other.getByRole("button", { name: "生成钥匙" }));
    await waitFor(() => expect(writes(s, "/settings/tokens")).toHaveLength(2));
    expect(writes(s, "/settings/tokens")[1].body).toEqual({ kind: "worker", label: "ci" });
    await userEvent.click(screen.getByRole("button", { name: "我已保存" }));
    expect(document.body.textContent).not.toContain("apa_dlg_SECRET");
  });
});

describe("设置 › Agent：接入其他 Agent", () => {
  // 「添加 Agent」的第一步选「接入新的程序」（原「接入其他 Agent」并到这里，界面设计 §4）。
  const open = async () => {
    await userEvent.click(await screen.findByRole("button", { name: "添加 Agent" }));
    await userEvent.selectOptions(screen.getByLabelText("用哪个程序"), "接入新的程序…");
    return within(screen.getByRole("region", { name: "接入新的程序" }));
  };

  it("逐项填写，命令下方实时拼出每一档的运行命令；提交的声明与手写 JSON 相同", async () => {
    const s = server({ live: [hermes(1, "active")] });
    mount(s.impl);
    const f = await open();
    await userEvent.type(f.getByLabelText("名字"), "克劳德");
    await userEvent.type(f.getByLabelText("英文名"), "Claude Code");
    await userEvent.type(f.getByLabelText("本机运行命令"), "claude -p ");
    await userEvent.click(f.getByRole("button", { name: "{prompt} 任务说明" }));
    await userEvent.click(f.getByRole("checkbox", { name: "只看" }));
    await userEvent.type(f.getByLabelText("只看 · 追加的参数，可不填"), "--permission-mode plan");
    await userEvent.click(f.getByRole("checkbox", { name: "可改文件" }));
    await userEvent.type(f.getByLabelText("可改文件 · 追加的参数，可不填"), "--permission-mode acceptEdits");
    await userEvent.click(f.getByRole("checkbox", { name: "需要时续接上次的会话" }));
    expect(f.getByRole("radio", { name: "它运行完会告诉 APA 会话编号" })).toBeChecked();
    await userEvent.type(f.getByLabelText("让它输出会话编号的参数"), "--output-format json");
    await userEvent.type(f.getByLabelText("续接参数"), "--resume {{session_id}");

    const pv = f.getByTestId("onboard-preview");
    expect(within(pv).getByRole("group", { name: "预览：" })).toBeInTheDocument();
    expect(pv).toHaveTextContent("新开：claude -p {prompt} --output-format json --permission-mode plan");
    await userEvent.click(within(pv).getByRole("button", { name: "可改文件" }));
    expect(pv).toHaveTextContent("续接：claude -p {prompt} --output-format json --permission-mode acceptEdits --resume {session_id}");
    expect(within(pv).getByRole("button", { name: "完全放开" })).toBeDisabled();

    await userEvent.click(f.getByRole("button", { name: "接入" }));
    await waitFor(() => expect(writes(s, "/settings/commands")).toHaveLength(1));
    expect(writes(s, "/settings/commands")[0].body).toEqual(JSON.parse(`{
      "harness": "claude-code",
      "name": {"zh": "克劳德", "en": "Claude Code"},
      "headless": {
        "command": ["claude", "-p", "{prompt}"],
        "output": ["--output-format", "json"],
        "session": {"extract": "$.session_id"},
        "resume": ["--resume", "{session_id}"],
        "access": {"read": ["--permission-mode", "plan"], "edit": ["--permission-mode", "acceptEdits"]}
      }
    }`));
  });

  it("填错时就地提示，不提交", async () => {
    const s = server({ live: [hermes(1, "active")] });
    mount(s.impl);
    const f = await open();
    await userEvent.type(f.getByLabelText("英文名"), "Hermes");
    expect(f.getByText(/已经有叫这个名字的 Agent/)).toBeInTheDocument();
    await userEvent.type(f.getByLabelText("本机运行命令"), "acme run");
    expect(f.getByText(/命令里要有 \{prompt\}，APA 用它放任务说明/)).toBeInTheDocument();
    await userEvent.type(f.getByLabelText("本机运行命令"), " {{token}");
    expect(f.getByText(/不认识的占位：\{token\}/)).toBeInTheDocument();
    await userEvent.click(f.getByRole("button", { name: "接入" }));
    expect(f.getByText(/至少勾一档权限/)).toBeInTheDocument();
    expect(f.getByText(/中文名和英文名都要填/)).toBeInTheDocument();
    expect(writes(s, "/settings/commands")).toHaveLength(0);
  });

  it("直接编辑声明与表单二选一：改为直接编辑后表单置灰，提交的是手改的 JSON", async () => {
    const s = server({ live: [hermes(1, "active")] });
    mount(s.impl);
    const f = await open();
    await userEvent.type(f.getByLabelText("英文名"), "Acme");
    await userEvent.click(f.getByText("直接编辑声明"));
    expect(f.getByTestId("onboard-json")).toHaveTextContent('"harness": "acme"');
    await userEvent.click(f.getByRole("button", { name: "改为直接编辑" }));
    expect(f.getByLabelText("英文名")).toBeDisabled();
    const box = f.getByRole("textbox", { name: "直接编辑声明" });
    await userEvent.clear(box);
    await userEvent.type(box, "{{bad");
    await userEvent.click(f.getByRole("button", { name: "接入" }));
    expect(f.getByText("声明不是合法的 JSON")).toBeInTheDocument();
    await userEvent.clear(box);
    await userEvent.type(box, '{{"harness":"x"}');
    await userEvent.click(f.getByRole("button", { name: "接入" }));
    await waitFor(() => expect(writes(s, "/settings/commands")).toEqual([{ method: "POST", url: "/v1/settings/commands", body: { harness: "x" } }]));
  });
});
