import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { features as items } from "./index";
import { feature as sources } from "../sources";
import { feature as materials } from "../materials/index";
import { readPrefill, registrable, linkTitle } from "./prefill";

// 测试边界替身：按 api/items、api/materials、GET /note-vaults、/source-events/{id} 的契约应答（附录 B.2、§12.8）。
// 授权、位置规整、同位置去重是服务端规则；替身只复现可观察行为，并把每次写入记下来供断言。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const AT = "2030-01-02T04:00:00Z";
const HUMAN = { type: "human", principal: "owner", via: "web" };

interface Mat { id: string; project: string; kind: string; title: string; form: string; link?: string; open_url?: string }
interface Rel { type: string; from: string; to: string }
interface Prog { id: string; text: string; actor: string; at: string; precision?: string }

const VAULT_OPEN = (path: string) => "obsidian://open?vault=acc-vault&file=" + encodeURIComponent(path);

function backend(seed: { items?: any[]; mats?: Mat[]; rels?: Rel[]; progress?: Record<string, Prog[]>; messages?: any[]; traces?: Record<string, any> } = {}) {
  const items: any[] = seed.items ?? [];
  const mats: Mat[] = seed.mats ?? [];
  const rels: Rel[] = seed.rels ?? [];
  const calls: { method: string; path: string; body?: any }[] = [];
  const projects = [{ id: "prj_work", name: "acc-work" }, { id: "prj_home", name: "acc-home" }];
  const vaults = [{ name: "acc-vault", open_url: "obsidian://open?vault={vault}&file={path}" }];
  let n = 0;
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body });
    const u = new URL(url, "http://x");
    const q = u.pathname;
    if (q === "/v1/auth/login") return json(200, { csrf_token: "tok-1", expires_at: "2031-01-01T00:00:00Z" });
    if (q === "/v1/projects") return json(200, { projects });
    if (q === "/v1/note-vaults") return json(200, { vaults });
    if (q === "/v1/items" && method === "GET") return json(200, { items });
    if (q === "/v1/items" && method === "POST") {
      const it = { id: "item_" + ++n, version: 1, ...body };
      items.push(it);
      return json(201, { id: it.id, version: 1 });
    }
    if (q === "/v1/materials" && method === "POST") {
      let m = mats.find((x) => x.project === body.project && x.link === body.link);
      const created = !m;
      if (!m) mats.push((m = { id: "mat_" + ++n, project: body.project, kind: body.kind ?? "reference", title: body.title, form: "link", link: body.link, open_url: body.link.startsWith("vault:acc-vault/") ? VAULT_OPEN(body.link.slice(16)) : undefined }));
      return json(created ? 201 : 200, { material: m.id, created });
    }
    let m = q.match(/^\/v1\/materials\/([^/]+)$/);
    if (m) {
      const mat = mats.find((x) => x.id === m![1]);
      return mat ? json(200, { material: mat, relations: [] }) : json(404, { code: "not_found", why: "no such material", next: "x" });
    }
    m = q.match(/^\/v1\/items\/([^/]+)\/(messages|relations|trace)$/);
    if (m) {
      const it = items.find((i) => i.id === m![1]);
      if (m[2] === "messages") return json(200, { messages: seed.messages ?? [] });
      if (m[2] === "trace") return json(200, seed.traces?.[m[1]] ?? { entries: [] });
      if (!it) return json(404, { code: "not_found", why: "no such item", next: "x" });
      if (body.remove) {
        const i = rels.findIndex((r) => r.from === it.id && r.to === body.to && r.type === body.type);
        if (i >= 0) rels.splice(i, 1);
      } else rels.push({ type: body.type, from: it.id, to: body.to });
      return json(200, { id: it.id, version: it.version });
    }
    m = q.match(/^\/v1\/items\/([^/]+)$/);
    if (m) {
      const it = items.find((i) => i.id === m![1]);
      if (!it) return json(404, { code: "not_found", why: "no such item", next: "x" });
      return json(200, { item: it, relations: rels.filter((r) => r.from === it.id || r.to === it.id), progress: seed.progress?.[it.id] ?? [], correlation_keys: [] });
    }
    const raw = q.match(/^\/v1\/source-events\/([^/]+)(\/trace)?$/);
    if (raw) {
      const ev = { id: raw[1], source: "src_mail", type: "mail.received", subject: "季度报表", sender_verified: true, disposition: "proposed", occurred_at: AT };
      if (raw[2]) return json(200, { message: ev, entries: [] });
      return json(200, { event: ev, content: { state: "full", bytes: 9, parts: [] }, body: "原文正文", scope: "full" });
    }
    return json(404, { code: "not_found", why: "no such route " + q, next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls, items, mats, rels, writes: () => calls.filter((c) => c.method !== "GET") };
}

function mount(fetchImpl: typeof fetch, path: string, authed = true) {
  if (authed) sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App features={[...items, sources, materials]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}

const field = (name: string) => screen.findByLabelText(name) as Promise<HTMLInputElement>;

beforeEach(() => sessionStorage.clear());

const PRE = "title=买票&body=第一行&project=acc-work&planned_for=2030-12-01&deadline=2030-12-03&link=" + encodeURIComponent("vault:acc-vault/行程.md");

describe("预填新建页（flow-prefill-new）", () => {
  it("ACT-PRE-01 未登录先到登录页，登录后回到填好的表单；此前没有任何写入", async () => {
    const b = backend();
    mount(b.impl, "/new?" + PRE, false);
    await userEvent.type(await screen.findByLabelText("口令"), "pw");
    expect(b.calls.filter((c) => c.path !== "/v1/auth/login")).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "登录" }));

    expect(await screen.findByText("内容由链接预填")).toBeInTheDocument();
    expect((await field("标题")).value).toBe("买票");
    expect((await field("正文")).value).toBe("第一行");
    expect((await field("计划日期")).value).toBe("2030-12-01");
    expect((await field("截止")).value).toBe("2030-12-03T23:59");
    await waitFor(async () => expect((await field("项目")).value).toBe("prj_work")); // 名字认成 id
    expect(screen.getByText("vault:acc-vault/行程.md")).toBeInTheDocument();
    expect(b.writes().filter((c) => c.path !== "/v1/auth/login")).toHaveLength(0); // 点创建之前账本不变
  });

  it("ACT-PRE-02 点创建才写入：事项带计划日期与截止，库内链接登记为项目的链接型材料并以依据关联", async () => {
    const b = backend();
    mount(b.impl, "/items/new?" + PRE);
    await waitFor(async () => expect((await field("项目")).value).toBe("prj_work"));
    expect(b.writes()).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "创建" }));

    await screen.findByRole("heading", { name: "买票", level: 1 });
    const w = b.writes();
    expect(w[0]).toMatchObject({ method: "POST", path: "/v1/items" });
    expect(w[0].body).toMatchObject({ kind: "task", phase: "open", title: "买票", body: "第一行", project: "prj_work", planned_for: "2030-12-01" });
    expect(Date.parse(w[0].body.deadline)).toBe(new Date("2030-12-03T23:59:59").getTime()); // 只给日期：当天 23:59:59
    expect(w[1]).toMatchObject({ method: "POST", path: "/v1/materials", body: { link: "vault:acc-vault/行程.md", title: "行程.md", project: "prj_work" } });
    expect(w[2]).toMatchObject({ path: "/v1/items/item_1/relations", body: { to: b.mats[0].id, type: "basis" } });
    // 详情的材料区：依据组里有这条链接型材料，“打开”是服务端渲染好的地址。
    const group = within(await screen.findByRole("group", { name: "依据" }));
    expect(group.getByRole("link", { name: "行程.md" })).toBeInTheDocument();
    expect(group.getByRole("link", { name: "打开" })).toHaveAttribute("href", VAULT_OPEN("行程.md"));
  });

  it("ACT-PRE-03 超长部分丢弃并提示，认不出的项目留空，非法位置写进正文末尾且不登记材料", async () => {
    const b = backend();
    const q = new URLSearchParams({ title: "题".repeat(250), body: "文".repeat(4500), project: "不存在的项目", link: "file:///tmp/a.md" });
    mount(b.impl, "/items/new?" + q);
    expect(((await field("标题")).value).length).toBe(200);
    expect(((await field("正文")).value).length).toBe(4000);
    expect(screen.getByText(/标题超过 200/)).toBeInTheDocument();
    expect(screen.getByText(/正文超过 4000/)).toBeInTheDocument();
    await waitFor(async () => expect((await field("项目")).value).toBe(""));
    await userEvent.click(screen.getByRole("button", { name: "创建" }));

    await waitFor(() => expect(b.items).toHaveLength(1));
    expect(b.items[0].project).toBeUndefined();
    expect(b.items[0].body).toBe("文".repeat(4000) + "\n\nfile:///tmp/a.md");
    expect(b.writes().some((c) => c.path === "/v1/materials")).toBe(false);
  });

  it("ACT-PRE-03 项目按 id 认出，网址登记为依据材料；没有项目时网址只写进正文", async () => {
    const b = backend();
    mount(b.impl, "/new?project=prj_home&title=x&link=" + encodeURIComponent("https://example.invalid/x"));
    await waitFor(async () => expect((await field("项目")).value).toBe("prj_home"));
    await userEvent.click(screen.getByRole("button", { name: "创建" }));
    await screen.findByRole("heading", { name: "x", level: 1 });
    expect(b.mats[0]).toMatchObject({ project: "prj_home", link: "https://example.invalid/x" });
    expect(b.rels).toEqual([{ type: "basis", from: "item_1", to: b.mats[0].id }]);
    expect(b.items[0].body).toBeUndefined();

    const b2 = backend();
    mount(b2.impl, "/new?title=y&link=" + encodeURIComponent("https://example.invalid/y"));
    await userEvent.click((await screen.findAllByRole("button", { name: "创建" })).at(-1)!);
    await waitFor(() => expect(b2.items).toHaveLength(1));
    expect(b2.items[0].body).toBe("https://example.invalid/y");
    expect(b2.mats).toHaveLength(0);
  });

  it("没有预填参数时不标“内容由链接预填”；空标题不发请求", async () => {
    const b = backend();
    mount(b.impl, "/new");
    await userEvent.click(await screen.findByRole("button", { name: "创建" }));
    expect(screen.queryByText("内容由链接预填")).toBeNull();
    expect(await screen.findByRole("alert")).toHaveTextContent("标题");
    expect(b.writes()).toHaveLength(0);
  });

  it("创建事项成功但材料登记被服务端拒绝：显示服务端原因，并给出新事项入口，不重复创建", async () => {
    const b = backend();
    const inner = b.impl;
    const impl = (async (url: string, init: RequestInit = {}) =>
      url === "/v1/materials" && init.method === "POST" ? json(400, { code: "invalid_request", why: "vault is not registered", next: "register it" }) : inner(url, init)) as unknown as typeof fetch;
    mount(impl, "/new?title=t&project=prj_work&link=" + encodeURIComponent("vault:acc-vault/a.md"));
    await waitFor(async () => expect((await field("项目")).value).toBe("prj_work"));
    await userEvent.click(screen.getByRole("button", { name: "创建" }));
    expect((await screen.findAllByText(/vault is not registered/)).length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: "打开新事项" })).toHaveAttribute("href", "/items/item_1");
    expect(b.items).toHaveLength(1);
  });

  it("解析：日期格式、带时区的截止、认不出的值", () => {
    const p = readPrefill(new URLSearchParams({ planned_for: "2030-13-45", deadline: "2030-12-03T09:30:00+08:00", title: "a" }));
    expect(p.plannedFor).toBe("");
    expect(p.deadline).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
    expect(readPrefill(new URLSearchParams({ deadline: "明天" })).deadline).toBe("");
    expect(registrable("vault:other/a.md", [{ name: "acc-vault" }])).toBe(false);
    expect(registrable("vault:acc-vault/../a.md", [{ name: "acc-vault" }])).toBe(false);
    expect(linkTitle("https://example.invalid/a/b%20c.md?x=1")).toBe("b c.md");
  });
});

describe("事项详情的材料区（flow-web-vault-materials、flow-report-links）", () => {
  const item = { id: "item_p", phase: "open", version: 3, kind: "task", title: "父事项", project: "prj_work" };
  const mats: Mat[] = [
    { id: "mat_r", project: "prj_work", kind: "reference", title: "报告.md", form: "link", link: "vault:acc-vault/产出/报告.md", open_url: VAULT_OPEN("产出/报告.md") },
    { id: "mat_b", project: "prj_work", kind: "reference", title: "外部依据", form: "link", link: "https://example.invalid/doc" },
    { id: "mat_m", project: "prj_work", kind: "minutes", title: "周会纪要", form: "owned" },
    { id: "mat_x", project: "prj_work", kind: "reference", title: "无打开方式", form: "link", link: "vault:gone/x.md" },
  ];
  const rels = (): Rel[] => [
    { type: "output", from: "item_p", to: "mat_r" },
    { type: "basis", from: "item_p", to: "mat_b" },
    { type: "minutes", from: "item_p", to: "mat_m" },
    { type: "output", from: "item_p", to: "mat_x" },
    { type: "relates", from: "item_p", to: "mat_b" },
  ];

  it("ACT-WVM-03 按依据、产出、纪要分组；链接型带“打开”（库内是 open_url，网址是网址本身，未登记打开方式的不给）", async () => {
    mount(backend({ items: [item], mats, rels: rels() }).impl, "/items/item_p");
    const out = within(await screen.findByRole("group", { name: "产出" }));
    expect(out.getByRole("link", { name: "打开" })).toHaveAttribute("href", VAULT_OPEN("产出/报告.md"));
    expect(out.getByText("该库未登记打开方式")).toBeInTheDocument();
    const basis = within(screen.getByRole("group", { name: "依据" }));
    expect(basis.getByRole("link", { name: "打开" })).toHaveAttribute("href", "https://example.invalid/doc");
    const minutes = within(screen.getByRole("group", { name: "纪要" }));
    expect(minutes.getByRole("link", { name: "周会纪要" })).toHaveAttribute("href", "/materials?id=mat_m");
    expect(minutes.queryByRole("link", { name: "打开" })).toBeNull(); // 自有型没有“打开”
    // 三类之外的关系仍在“关系”里，不进材料区。
    expect(within(screen.getByRole("list", { name: "关系" })).getAllByRole("listitem")).toHaveLength(1);
  });

  it("ACT-RPL-04 移除自动建立的产出关系：发 remove 的关系请求，材料本身保留，页面按服务端重读", async () => {
    const b = backend({ items: [item], mats, rels: rels() });
    mount(b.impl, "/items/item_p");
    await userEvent.click(await screen.findByRole("button", { name: "移除产出：报告.md" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "移除产出：报告.md" })).toBeNull());
    expect(b.writes()).toEqual([{ method: "POST", path: "/v1/items/item_p/relations", body: { to: "mat_r", type: "output", remove: true } }]);
    expect(b.rels.some((r) => r.to === "mat_r")).toBe(false);
    expect(b.mats.some((m) => m.id === "mat_r")).toBe(true); // 材料没有被删
    expect(screen.getByRole("button", { name: "移除产出：无打开方式" })).toBeInTheDocument();
  });

  it("没有材料关系时不显示材料区", async () => {
    mount(backend({ items: [item] }).impl, "/items/item_p");
    await screen.findByRole("heading", { name: "父事项" });
    expect(screen.queryByRole("region", { name: "材料" })).toBeNull();
  });
});

describe("来源区链到来源原文页（flow-web-raw-page ACT-WRP-04）", () => {
  it("事项挂靠消息的链接打开 /sources/events/<id>，落在原文页", async () => {
    const msg = { id: "sev_mail", source: "src_mail", sender: "bob@example.com", sender_verified: true, forward_allowed: true, subject: "季度报表", summary: "", attach_state: "attached", link: "" };
    mount(backend({ items: [{ id: "item_m", phase: "open", version: 1, kind: "task", title: "回复报表" }], messages: [msg] }).impl, "/items/item_m");
    const link = await screen.findByRole("link", { name: "处理经过" });
    expect(link).toHaveAttribute("href", "/sources/events/sev_mail");
    await userEvent.click(link);
    expect(await screen.findByRole("heading", { name: "季度报表", level: 1 })).toBeInTheDocument();
    expect(await screen.findByText("原文正文")).toBeInTheDocument();
  });

  it("事项经过里来源消息的标题链接到同一个原文页", async () => {
    const trace = {
      origins: [{ message: { id: "sev_o", source: "src_mail", type: "mail.received", subject: "来源邮件", sender_verified: true, disposition: "proposed", occurred_at: AT }, entries: [{ at: AT, who: { kind: "source", name: "src_mail" }, kind: "arrived", args: {}, steps: [] }] }],
      entries: [],
    };
    mount(backend({ items: [{ id: "item_t", phase: "open", version: 1, kind: "task", title: "t" }], traces: { item_t: trace } }).impl, "/items/item_t");
    expect(await screen.findByRole("link", { name: "收到「来源邮件」" })).toHaveAttribute("href", "/sources/events/sev_o");
  });
});

describe("导入条目只显示到日（flow-web-import ACT-WIM-04）", () => {
  const day = (id: string, text: string, at: string): Prog => ({ id, text, actor: "owner", at, precision: "day" });
  const noon = (d: string) => new Date(d + "T12:00:00").toISOString();
  const progress: Prog[] = [
    // 写入顺序与发生顺序不同：时间线按 at 排。
    { id: "p4", text: "精确二", actor: "owner", at: new Date("2030-02-05T15:42:00").toISOString() },
    day("p2", "约定二", noon("2030-02-02")),
    day("p1", "约定一", noon("2030-02-01")),
    { id: "p3", text: "精确一", actor: "owner", at: new Date("2030-02-03T09:07:00").toISOString() },
    day("p5", "约定三", noon("2030-02-04")),
  ];

  it("precision 为 day 的进展只显示日期，精确时刻的显示时刻，按 at 先后排列", async () => {
    mount(backend({ items: [{ id: "item_i", phase: "done", version: 2, kind: "task", title: "T-0001" }], progress: { item_i: progress } }).impl, "/items/item_i");
    // 讨论流里的每条发言：正文与时间分开读（作者名在前）。
    const rows = within(await screen.findByRole("list", { name: "讨论" })).getAllByRole("listitem");
    const text = (r: HTMLElement) => r.querySelector(".comment-text")!.textContent!;
    const time = (r: HTMLElement) => r.querySelector("time")!.textContent!;
    expect(rows.map((r) => text(r).slice(0, 3))).toEqual(["约定一", "约定二", "精确一", "约定三", "精确二"]);
    for (const i of [0, 1, 3]) expect(time(rows[i])).not.toMatch(/\d{1,2}:\d{2}/);
    for (const i of [2, 4]) expect(time(rows[i])).toMatch(/\d{1,2}:\d{2}/);
    expect(time(rows[0])).toContain(new Date(noon("2030-02-01")).toLocaleDateString("zh-CN"));
  });

  it("经过里 precision 为 day 的阶段变更只显示日期，其余显示时刻，且按 at 排列", async () => {
    const step = (precision?: string) => ({ seq: 1, type: "item.phase_changed", object: "item_i", actor: HUMAN, at: AT, payload: precision ? { precision } : {} });
    const done = { at: new Date("2030-02-06T18:00:00").toISOString(), who: { kind: "owner" }, kind: "phase", args: { to: "done" }, steps: [step("day")] };
    const reopened = { at: new Date("2030-02-07T10:15:00").toISOString(), who: { kind: "owner" }, kind: "phase", args: { to: "open" }, steps: [step()] };
    mount(backend({ items: [{ id: "item_i", phase: "open", version: 2, kind: "task", title: "T-0001" }], traces: { item_i: { entries: [reopened, done] } } }).impl, "/items/item_i");
    const rows = within(await screen.findByRole("list", { name: "处理经过" })).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0].querySelector("time")!.textContent).not.toMatch(/:/); // 先发生的完成转移：只到日
    expect(rows[0].querySelector("time")!.getAttribute("title")).not.toMatch(/18:00/);
    expect(rows[1].querySelector("time")!.textContent).toMatch(/10:15/);
  });
});
