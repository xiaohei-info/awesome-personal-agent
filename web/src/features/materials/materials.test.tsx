import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { diffLines } from "./MaterialsPage";

// 测试边界替身：按 api/materials 的契约应答（登记、修订历史、commit、confirm/reject/revert、搜索；错误体见附录 B.3）。
// 写入策略、基线冲突、授权都是服务端规则——替身只复现它的可观察行为，页面只能如实展示。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string, next = "fix the request") => json(status, { code, why, next });

interface Mat { id: string; project: string; kind: string; title: string; current_rev?: string; location?: string }
interface Rev { id: string; material: string; seq: number; content: string; actor: { type: string; principal: string }; reason: string; based_on: string; state: string; at: string }

const rev = (id: string, material: string, seq: number, content: string, state: string, based_on = "", reason = "r" + seq): Rev => ({
  id, material, seq, content, state, based_on, reason, actor: { type: seq % 2 ? "agent" : "human", principal: seq % 2 ? "bot" : "owner" }, at: "2030-01-0" + seq + "T00:00:00Z",
});

function backend(o: { materials?: Mat[]; revisions?: Rev[]; hits?: any[]; proposedKinds?: string[]; relations?: { type: string; from: string; to: string }[] } = {}) {
  const materials = o.materials ?? [];
  const revisions = o.revisions ?? [];
  const proposedKinds = o.proposedKinds ?? ["requirement", "design", "guideline"];
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    const u = new URL(url, "http://x");
    const q = u.pathname;
    if (q === "/v1/projects") return json(200, { projects: [{ id: "acc-open", name: "acc-open" }, { id: "acc-home", name: "acc-home" }] });
    if (q === "/v1/materials" && method === "GET") {
      const p = u.searchParams.get("project");
      return json(200, { materials: materials.filter((m) => p === null || p === "" || m.project === p) });
    }
    if (q === "/v1/items") return json(200, { items: [{ id: "item_1", title: "登录改造" }] });
    if (q === "/v1/settings/policy") return json(200, { version: 1, material_write: { default: "proposed", kinds: { minutes: "direct", design: "proposed" } } });
    if (q === "/v1/materials/search") return json(200, { hits: o.hits ?? [], ...{ _q: u.search } });
    if (q === "/v1/materials" && method === "POST") {
      const id = "mat_new" + materials.length;
      const state = proposedKinds.includes(body.kind) ? "proposed" : "current";
      materials.push({ id, project: body.project, kind: body.kind, title: body.title, current_rev: state === "current" ? "rev_" + id : "" });
      revisions.push(rev("rev_" + id, id, 1, body.content, state));
      return json(201, { material: id, revision: "rev_" + id, state });
    }
    const m = q.match(/^\/v1\/materials\/([^/]+)(\/.*)?$/);
    if (m) {
      const mat = materials.find((x) => x.id === m[1]);
      if (!mat) return err(404, "not_found", "material does not exist");
      const revs = revisions.filter((r) => r.material === mat.id);
      if (!m[2]) {
        if (mat.location) return json(200, { material: mat, hosted: { repo: "r", path: "p" } });
        const cur = revs.find((r) => r.id === mat.current_rev);
        return json(200, { material: mat, revision: cur ?? null, relations: (o.relations ?? []).filter((r) => r.to === mat.id) });
      }
      if (m[2] === "/revisions" && method === "GET") return json(200, { revisions: revs });
      if (m[2] === "/revisions" && method === "POST") {
        if (body.id) return err(400, "invalid_request", "id comes from the URL");
        const seq = revs.length + 1;
        const state = proposedKinds.includes(mat.kind) ? "proposed" : "current";
        if (state === "current" && body.based_on !== (mat.current_rev ?? "")) return err(409, "conflict", "baseline changed", "re-read and retry");
        const r = rev("rev_" + mat.id + seq, mat.id, seq, body.content, state, body.based_on, body.reason);
        revisions.push(r);
        if (state === "current") {
          revs.forEach((x) => x.state === "current" && (x.state = "superseded"));
          r.state = "current";
          mat.current_rev = r.id;
        }
        return json(201, { material: mat.id, revision: r.id, state });
      }
      if (m[2] === "/revert") {
        const to = revs.find((r) => r.id === body.to)!;
        const seq = revs.length + 1;
        const r = rev("rev_" + mat.id + seq, mat.id, seq, to.content, "current", mat.current_rev, body.reason);
        revs.forEach((x) => x.state === "current" && (x.state = "superseded"));
        revisions.push(r);
        mat.current_rev = r.id;
        return json(200, { material: mat.id, revision: r.id, state: "current" });
      }
    }
    const c = q.match(/^\/v1\/revisions\/([^/]+)\/(confirm|reject)$/);
    if (c && method === "POST") {
      const r = revisions.find((x) => x.id === c[1])!;
      const mat = materials.find((x) => x.id === r.material)!;
      if (r.state !== "proposed") return err(409, "conflict", "revision is already " + r.state);
      if (c[2] === "reject") {
        r.state = "superseded";
        return json(200, { material: mat.id, revision: r.id, state: "superseded" });
      }
      const stale = r.based_on !== (mat.current_rev ?? "");
      if (stale && !body.on_stale) return err(409, "conflict", "baseline changed: the current revision is no longer the one this proposal was based on");
      if (stale && body.on_stale === "rebase") {
        r.state = "superseded";
        const n = rev("rev_re" + r.id, mat.id, revisions.length + 1, r.content, "proposed", mat.current_rev);
        revisions.push(n);
        return json(200, { material: mat.id, revision: n.id, state: "proposed" });
      }
      revisions.filter((x) => x.material === mat.id && x.state === "current").forEach((x) => (x.state = "superseded"));
      r.state = "current";
      mat.current_rev = r.id;
      return json(200, { material: mat.id, revision: r.id, state: "current" });
    }
    return err(404, "not_found", "no such route");
  }) as unknown as typeof fetch;
  return { impl, calls, materials, revisions };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/materials"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const note = (): Mat => ({ id: "mat_note", project: "acc-open", kind: "minutes", title: "周会纪要", current_rev: "r3" });
const noteRevs = () => [
  rev("r1", "mat_note", 1, "a\nb", "superseded"),
  rev("r2", "mat_note", 2, "a\nb\nc", "superseded", "r1"),
  rev("r3", "mat_note", 3, "a\nB\nc", "current", "r2"),
];
const design: Mat = { id: "mat_design", project: "acc-open", kind: "design", title: "登录方案", current_rev: "d1" };
const open = async (title: string) => {
  await userEvent.click(await screen.findByRole("button", { name: title }));
  return screen.findByRole("region", { name: "材料详情" });
};

beforeEach(() => sessionStorage.clear());

describe("diffLines", () => {
  it("按行给出不变、删除、新增", () => {
    expect(diffLines("a\nb\nc", "a\nB\nc")).toEqual([
      { op: " ", text: "a" },
      { op: "-", text: "b" },
      { op: "+", text: "B" },
      { op: " ", text: "c" },
    ]);
  });
});

describe("版本列表与 diff（ACT-MAT-04）", () => {
  it("列出每条修订的 actor、reason、based_on 与状态；diff 对比上一条；旧内容原样可看", async () => {
    mount(backend({ materials: [note()], revisions: noteRevs() }).impl);
    const d = within(await open("周会纪要"));
    const list = within(await d.findByRole("region", { name: "版本列表" }));
    const rows = list.getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent(/#3.*current.*bot.*r3.*基于 r2/s);
    expect(rows[2]).toHaveTextContent(/#1.*superseded/s);
    await userEvent.click(within(rows[0]).getByRole("button", { name: "查看改动" }));
    const diff = within(await screen.findByTestId("diff-r3"));
    expect(diff.getByText("B").closest("[data-op]")).toHaveAttribute("data-op", "+");
    expect(diff.getByText("b").closest("[data-op]")).toHaveAttribute("data-op", "-");
    await userEvent.click(within(rows[2]).getByRole("button", { name: "查看内容" }));
    expect(screen.getByTestId("content-r1")).toHaveTextContent(/a\s*b/);
  });
});

describe("编辑与写入策略（ACT-MAT-11）", () => {
  it("direct 类：保存后直接成为 current；带 based_on 与 CSRF", async () => {
    const b = backend({ materials: [note()], revisions: noteRevs() });
    mount(b.impl);
    const d = within(await open("周会纪要"));
    const ed = await d.findByLabelText("Markdown 内容");
    expect(ed).toHaveValue("a\nB\nc");
    await userEvent.clear(ed);
    await userEvent.type(ed, "新正文");
    await userEvent.type(d.getByLabelText("修改原因"), "补充");
    await userEvent.click(d.getByRole("button", { name: "保存修改" }));
    expect(await screen.findByRole("status")).toHaveTextContent("已生效");
    const post = b.calls.find((c) => c.method === "POST" && c.path === "/v1/materials/mat_note/revisions")!;
    expect(post.body).toMatchObject({ content: "新正文", reason: "补充", based_on: "r3" });
    expect(post.csrf).toBe("tok-1");
  });

  it("policy 为 proposed 的 kind：保存后提示进入确认流，当前内容不变", async () => {
    const b = backend({ materials: [design], revisions: [rev("d1", "mat_design", 1, "v1", "current")] });
    mount(b.impl);
    const d = within(await open("登录方案"));
    const ed = await d.findByLabelText("Markdown 内容");
    await userEvent.type(ed, "+改");
    await userEvent.type(d.getByLabelText("修改原因"), "x");
    await userEvent.click(d.getByRole("button", { name: "保存修改" }));
    expect(await screen.findByRole("status")).toHaveTextContent("等你确认后生效");
    expect(b.materials[0].current_rev).toBe("d1");
  });

  it("direct 基线冲突：显示服务端冲突原因，不假装成功", async () => {
    const b = backend({ materials: [note()], revisions: noteRevs() });
    mount(b.impl);
    const d = within(await open("周会纪要"));
    await userEvent.type(await d.findByLabelText("Markdown 内容"), "x");
    b.materials[0].current_rev = "r-other"; // 别处已写入
    await userEvent.click(d.getByRole("button", { name: "保存修改" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("baseline changed");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});

describe("未保存草稿不被父组件重渲染冲掉", () => {
  it("详情有未保存草稿时在搜索框输入、切换筛选，草稿保持", async () => {
    mount(backend({ materials: [note()], revisions: noteRevs() }).impl);
    const d = within(await open("周会纪要"));
    const ed = await d.findByLabelText("Markdown 内容");
    await userEvent.type(ed, "-草稿");
    await userEvent.type(screen.getByLabelText("搜索材料"), "超时");
    await userEvent.selectOptions(screen.getByLabelText("按类型筛选"), "minutes");
    await userEvent.selectOptions(screen.getByLabelText("按项目筛选"), "acc-open");
    expect(d.getByLabelText("Markdown 内容")).toHaveValue("a\nB\nc-草稿");
  });

  it("保存返回 409 后草稿与原因保持，并显示服务端原因", async () => {
    const b = backend({ materials: [note()], revisions: noteRevs() });
    mount(b.impl);
    const d = within(await open("周会纪要"));
    await userEvent.type(await d.findByLabelText("Markdown 内容"), "-草稿");
    await userEvent.type(d.getByLabelText("修改原因"), "补充");
    b.materials[0].current_rev = "r-other";
    await userEvent.click(d.getByRole("button", { name: "保存修改" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("baseline changed");
    expect(d.getByLabelText("Markdown 内容")).toHaveValue("a\nB\nc-草稿");
    expect(d.getByLabelText("修改原因")).toHaveValue("补充");
  });
});

describe("proposed 修订确认（ACT-MAT-03 / ACT-MAT-06 / ACT-AGM-01）", () => {
  const withProposed = () => {
    const p = rev("p1", "mat_design", 2, "v2", "proposed", "d1");
    return backend({ materials: [{ ...design }], revisions: [rev("d1", "mat_design", 1, "v1", "current"), p] });
  };

  it("确认：成为 current，旧修订变 superseded；diff 对比当前", async () => {
    const b = withProposed();
    mount(b.impl);
    const d = within(await open("登录方案"));
    const box = within(await d.findByRole("region", { name: "等你确认的修改" }));
    expect(box.queryByText(/确认前这份材料又被改过/)).not.toBeInTheDocument();
    expect(box.getByTestId("diff-p1")).toBeInTheDocument();
    await userEvent.click(box.getByRole("button", { name: "确认生效" }));
    expect(await screen.findByRole("status")).toHaveTextContent("已确认");
    expect(b.revisions.map((r) => r.state)).toEqual(["superseded", "current"]);
    expect(b.calls.find((c) => c.path === "/v1/revisions/p1/confirm")!.csrf).toBe("tok-1");
  });

  it("拒绝：不生效，内容仍保留在版本列表", async () => {
    const b = withProposed();
    mount(b.impl);
    const d = within(await open("登录方案"));
    await userEvent.click(await d.findByRole("button", { name: "拒绝" }));
    await screen.findByRole("status");
    expect(screen.getByRole("status")).toHaveTextContent("已拒绝");
    expect(d.queryByRole("region", { name: "等你确认的修改" })).not.toBeInTheDocument();
    expect(b.revisions.find((r) => r.id === "p1")).toMatchObject({ state: "superseded", content: "v2" });
    expect(d.getAllByRole("listitem")).toHaveLength(2);
  });

  it("基线已变化：提示并强制二选一；仍然采用 → on_stale=adopt", async () => {
    const b = withProposed();
    b.revisions.push(rev("d9", "mat_design", 3, "v-newer", "current", "d1"));
    b.revisions[0].state = "superseded";
    b.materials[0].current_rev = "d9";
    mount(b.impl);
    const d = within(await open("登录方案"));
    const box = within(await d.findByRole("region", { name: "等你确认的修改" }));
    expect(box.getByText(/确认前这份材料又被改过/)).toBeInTheDocument();
    expect(box.queryByRole("button", { name: "确认生效" })).not.toBeInTheDocument();
    await userEvent.click(box.getByRole("button", { name: "仍用这版" }));
    await screen.findByRole("status");
    expect(b.calls.find((c) => c.path === "/v1/revisions/p1/confirm")!.body).toMatchObject({ on_stale: "adopt" });
    expect(b.revisions.find((r) => r.id === "p1")!.state).toBe("current");
  });

  it("基线已变化：基于当前重提 → on_stale=rebase，产生新的待确认修订", async () => {
    const b = withProposed();
    b.revisions.push(rev("d9", "mat_design", 3, "v-newer", "current", "d1"));
    b.materials[0].current_rev = "d9";
    mount(b.impl);
    const d = within(await open("登录方案"));
    const box = within(await d.findByRole("region", { name: "等你确认的修改" }));
    await userEvent.click(box.getByRole("button", { name: "在最新版上重做" }));
    await screen.findByRole("status");
    expect(b.calls.find((c) => c.path === "/v1/revisions/p1/confirm")!.body).toMatchObject({ on_stale: "rebase" });
    expect(b.revisions.filter((r) => r.state === "proposed")).toHaveLength(1);
    expect(await d.findByRole("region", { name: "等你确认的修改" })).toBeInTheDocument();
  });

  it("确认被服务端拒绝（如 delegate/已处置）：显示原因，状态不变", async () => {
    const b = withProposed();
    mount(b.impl);
    const d = within(await open("登录方案"));
    const box = within(await d.findByRole("region", { name: "等你确认的修改" }));
    b.revisions[1].state = "superseded"; // 别处已处置
    await userEvent.click(box.getByRole("button", { name: "确认生效" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("already superseded");
  });
});

describe("新建规范（ACT-AGM-01 / ACT-NRG-01）", () => {
  it("无仓库项目的规范只填处理规则也能提交；提示待确认而不是已生效", async () => {
    const b = backend();
    mount(b.impl);
    await userEvent.click(await screen.findByRole("button", { name: "新建材料" }));
    const f = within(await screen.findByRole("form", { name: "新建材料" }));
    await userEvent.selectOptions(f.getByLabelText("类型"), "guideline");
    await userEvent.selectOptions(f.getByLabelText("项目"), "acc-home");
    await userEvent.type(f.getByLabelText("标题"), "家庭事务规范");
    const body = f.getByLabelText("Markdown 内容") as HTMLTextAreaElement;
    for (const h of ["处理规则", "架构边界", "开发规范", "设计约束", "验收要求"]) expect(body.value).toContain("## " + h);
    await userEvent.type(body, "先问我");
    await userEvent.type(f.getByLabelText("修改原因"), "初版");
    await userEvent.click(f.getByRole("button", { name: "提交给自己确认" }));
    expect(await screen.findByRole("status")).toHaveTextContent("等你确认后生效");
    const post = b.calls.find((c) => c.method === "POST" && c.path === "/v1/materials")!;
    expect(post.body).toMatchObject({ kind: "guideline", project: "acc-home", title: "家庭事务规范", reason: "初版", based_on: "" });
    expect(post.body.content).toContain("先问我");
  });

  it("提交按钮按写入策略预告后果：直接生效 / 提交给自己确认；规范恒为先确认；没设过的类型不猜", async () => {
    mount(backend().impl);
    await userEvent.click(await screen.findByRole("button", { name: "新建材料" }));
    const f = within(await screen.findByRole("form", { name: "新建材料" }));
    expect(await f.findByRole("button", { name: "保存（直接生效）" })).toBeInTheDocument(); // 默认类型是纪要
    await userEvent.selectOptions(f.getByLabelText("类型"), "design");
    expect(f.getByRole("button", { name: "提交给自己确认" })).toBeInTheDocument();
    await userEvent.selectOptions(f.getByLabelText("类型"), "guideline");
    expect(f.getByRole("button", { name: "提交给自己确认" })).toBeInTheDocument();
    await userEvent.selectOptions(f.getByLabelText("类型"), "reference");
    expect(f.getByRole("button", { name: "提交" })).toBeInTheDocument();
  });

  it("服务端拒绝新建时显示原因", async () => {
    mount((async (url: string, init: RequestInit = {}) => {
      if (init.method === "POST") return err(409, "conflict", "project already has a guideline");
      return url === "/v1/projects" ? json(200, { projects: [] }) : json(200, { materials: [] });
    }) as unknown as typeof fetch);
    await userEvent.click(await screen.findByRole("button", { name: "新建材料" }));
    const f = within(await screen.findByRole("form", { name: "新建材料" }));
    await userEvent.type(f.getByLabelText("标题"), "t");
    await userEvent.type(f.getByLabelText("Markdown 内容"), "x");
    await userEvent.type(f.getByLabelText("修改原因"), "x");
    await userEvent.click(f.getByRole("button", { name: "提交" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("project already has a guideline");
  });
});

describe("回退（ACT-MAT-07）", () => {
  it("在 Web 回退：新增一条内容等于旧快照的修订，历史不改", async () => {
    const b = backend({ materials: [note()], revisions: noteRevs() });
    mount(b.impl);
    const d = within(await open("周会纪要"));
    const rows = within(await d.findByRole("region", { name: "版本列表" })).getAllByRole("listitem");
    expect(within(rows[0]).queryByRole("button", { name: "回退到此版" })).not.toBeInTheDocument(); // 当前版不能回退到自己
    await userEvent.click(within(rows[2]).getByRole("button", { name: "回退到此版" }));
    await screen.findByText("已回退");
    expect(b.calls.find((c) => c.path === "/v1/materials/mat_note/revert")!.body).toMatchObject({ to: "r1" });
    expect(b.revisions).toHaveLength(4);
    expect(b.revisions[3].content).toBe("a\nb");
    expect(b.revisions[0].content).toBe("a\nb");
    expect(b.revisions[2].content).toBe("a\nB\nc");
  });
});

describe("关联事项（ACT-MAT-13）", () => {
  it("材料详情显示中文关系类型与事项标题，点击进事项页；无关联不显示该区块", async () => {
    mount(backend({ materials: [note(), design], revisions: noteRevs(), relations: [{ type: "basis", from: "item_1", to: "mat_note" }] }).impl);
    const r = within(within(await open("周会纪要")).getByRole("region", { name: "关联事项" }));
    expect(await r.findByText(/依据/)).toBeInTheDocument();
    expect(r.getByRole("link", { name: "登录改造" })).toHaveAttribute("href", "/items/item_1");
    expect(within(await open("登录方案")).queryByRole("region", { name: "关联事项" })).not.toBeInTheDocument();
  });
});

describe("检索与筛选（ACT-MAT-09 / ACT-MAT-13）", () => {
  it("搜索框走 /materials/search，只显示 id、标题、片段；中文词原样透传", async () => {
    const b = backend({ materials: [note()], hits: [{ id: "mat_x", title: "登录排查", kind: "worknote", project: "acc-open", hosted: false, snippet: "…登录超时的根因…" }] });
    mount(b.impl);
    await userEvent.type(await screen.findByLabelText("搜索材料"), "超时");
    await userEvent.click(screen.getByRole("button", { name: "搜索" }));
    const hit = await screen.findByRole("listitem", { name: "mat_x" });
    expect(hit).toHaveTextContent("登录排查");
    expect(hit).toHaveTextContent("登录超时的根因");
    expect(b.calls.some((c) => c.path.startsWith("/v1/materials/search?") && new URL(c.path, "http://x").searchParams.get("q") === "超时")).toBe(true);
  });

  it("按项目、kind、关联事项筛选；托管型命中标明来源与路径", async () => {
    const b = backend({
      materials: [note(), design],
      hits: [{ id: "mat_h", title: "仓库设计", kind: "design", project: "acc-open", hosted: true, path: "docs/a.md" }],
    });
    mount(b.impl);
    await screen.findByRole("button", { name: "周会纪要" });
    await userEvent.selectOptions(screen.getByLabelText("按类型筛选"), "design");
    expect(screen.queryByRole("button", { name: "周会纪要" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "登录方案" })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("关联事项"), "item_1");
    await userEvent.click(screen.getByRole("button", { name: "搜索" }));
    const hit = await screen.findByRole("listitem", { name: "mat_h" });
    expect(hit).toHaveTextContent("在仓库里（只读）");
    expect(hit).toHaveTextContent("docs/a.md");
    const call = b.calls.find((c) => c.path.startsWith("/v1/materials/search?"))!;
    const sp = new URL(call.path, "http://x").searchParams;
    expect(sp.get("item")).toBe("item_1");
    expect(sp.get("kind")).toBe("design");
    await userEvent.selectOptions(screen.getByLabelText("按项目筛选"), "acc-home");
    expect(b.calls.some((c) => c.path === "/v1/materials?project=acc-home")).toBe(true);
  });

  it("托管型材料在此页只显示登记信息，不提供编辑", async () => {
    const hosted: Mat = { id: "mat_hosted", project: "acc-open", kind: "design", title: "仓库文档", location: "r:p" };
    mount(backend({ materials: [hosted] }).impl);
    const d = within(await open("仓库文档"));
    expect(await d.findByText(/在仓库里（只读）/)).toBeInTheDocument();
    expect(d.queryByLabelText("Markdown 内容")).not.toBeInTheDocument();
  });
});

describe("读取失败", () => {
  it("材料列表读取失败：显示服务端原因，不显示空状态", async () => {
    mount((async (url: string) => (url === "/v1/projects" ? json(200, { projects: [] }) : err(500, "internal", "store is unavailable"))) as unknown as typeof fetch);
    expect(await screen.findByRole("alert")).toHaveTextContent("store is unavailable");
    expect(screen.queryByText("还没有材料")).not.toBeInTheDocument();
  });

  it("没有材料时显示空状态", async () => {
    mount(backend().impl);
    expect(await screen.findByText("还没有材料")).toBeInTheDocument();
  });
});
