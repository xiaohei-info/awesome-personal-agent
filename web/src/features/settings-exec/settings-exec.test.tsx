import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/settings/exec 的契约应答（声明确认前不进清单/命令；确认后生效；写请求带 CSRF）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix the input" });

interface Exec { id: string; kind: string; host?: string; scenarios?: string[] }
interface Rec { harness: string; rev: number; state: string; declaration: { harness: string; headless?: Record<string, unknown>; main_session?: Record<string, unknown>; executors?: Exec[] } }

const decl = (harness: string, cmd: string[], executors: Exec[] = []): Rec["declaration"] => ({ harness, headless: { command: cmd }, executors });

// 第二个 describe 用：带 access.full / resume / main_session 的完整声明。
const full: Rec = { harness: "deep", rev: 1, state: "pending", declaration: { harness: "deep", headless: { command: ["/opt/deep/run"], resume: ["/opt/deep/resume", "--last"], access: { read: ["--ro"], full: ["--dangerously-all"] } }, main_session: { launch: "wake", args: ["--wake-flag"] }, executors: [{ id: "deep.cli", kind: "external-cli" }] } };

function server(o: { recs?: Rec[]; execs?: Exec[]; deny?: boolean } = {}) {
  let execs = o.execs ?? [];
  const recs = o.recs ?? [];
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (o.deny && url.startsWith("/v1/settings") && method !== "GET") return err(403, "web_only", "settings can only be used by the owner in the Web UI");
    if (url === "/v1/settings/executors" && method === "GET") return json(200, { executors: execs });
    if (url === "/v1/settings/executors" && method === "POST") {
      if (!body.id) return err(400, "invalid_request", "id is required");
      execs = [...execs.filter((e) => e.id !== body.id), body];
      return json(200, body);
    }
    const rm = url.match(/^\/v1\/settings\/executors\/(.+)$/);
    if (rm && method === "DELETE") {
      if (!execs.some((e) => e.id === decodeURIComponent(rm[1]))) return err(404, "not_found", "executor not found");
      execs = execs.filter((e) => e.id !== decodeURIComponent(rm[1]));
      return json(200, { id: rm[1] });
    }
    if (url === "/v1/settings/declarations") return json(200, { declarations: recs.filter((r) => r.state === "pending") });
    const d = url.match(/^\/v1\/settings\/declarations\/([^/]+)\/(\d+)\/(confirm|reject)$/);
    if (d && method === "POST") {
      const r = recs.find((x) => x.harness === d[1] && x.rev === Number(d[2]));
      if (!r) return err(404, "not_found", "declaration not found");
      if (r.state !== "pending") return err(409, "conflict", `declaration is ${r.state}, not pending`);
      r.state = d[3] === "confirm" ? "active" : "rejected";
      if (r.state === "active") execs = [...execs, ...(r.declaration.executors ?? [])];
      return json(200, {});
    }
    if (url === "/v1/settings/commands" && method === "GET") return json(200, { declarations: recs.filter((r) => r.state === "active") });
    if (url === "/v1/settings/commands" && method === "POST") {
      if (body.bogus) return err(400, "invalid_request", "unknown field bogus");
      recs.push({ harness: body.harness, rev: 1, state: "active", declaration: body });
      return json(201, {});
    }
    return err(404, "not_found", "no such route");
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings-exec"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const section = (name: string) => screen.getByRole("region", { name });
const pending = (harness: string, rev = 1): Rec => ({ harness, rev, state: "pending", declaration: decl(harness, ["/opt/" + harness + "/run", "--go"], [{ id: harness + ".build", kind: "external-cli" }]) });

beforeEach(() => sessionStorage.clear());

describe("执行者清单（flow-env-baseline）", () => {
  it("列出清单；新增外部 CLI 执行者 → POST 带 CSRF 与字段，列表以服务端为准", async () => {
    const s = server({ execs: [{ id: "work-host.cli", kind: "external-cli", host: "m1" }] });
    mount(s.impl);
    await screen.findByText("work-host.cli");
    const sec = section("可以派活的对象");
    await userEvent.type(within(sec).getByLabelText("名称"), "other-host.cli");
    await userEvent.selectOptions(within(sec).getByLabelText("种类"), "external-cli");
    await userEvent.type(within(sec).getByLabelText("在哪台机器（可选）"), "m2");
    await userEvent.type(within(sec).getByRole("textbox", { name: "适合做什么" }), "code, review{Enter}");
    await userEvent.click(within(sec).getByRole("button", { name: "添加" }));

    expect(await within(sec).findByText("other-host.cli")).toBeInTheDocument();
    const w = s.calls.find((c) => c.method === "POST")!;
    expect(w.path).toBe("/v1/settings/executors");
    expect(w.csrf).toBe("tok-1");
    expect(w.body).toEqual({ id: "other-host.cli", kind: "external-cli", host: "m2", scenarios: ["code", "review"] });
  });

  it("删除执行者 → DELETE；服务端拒绝时显示原因", async () => {
    const s = server({ execs: [{ id: "a.cli", kind: "external-cli" }] });
    mount(s.impl);
    const li = (await screen.findByText("a.cli")).closest("li")!;
    await userEvent.click(within(li).getByRole("button", { name: "删除" }));
    expect(await screen.findByText("还没有执行者")).toBeInTheDocument();
    expect(s.calls.find((c) => c.method === "DELETE")!.path).toBe("/v1/settings/executors/a.cli");
  });

  it("被服务端拒绝（web_only）时展示 why，不假装成功", async () => {
    const s = server({ deny: true });
    mount(s.impl);
    const sec = await screen.findByRole("region", { name: "可以派活的对象" });
    await userEvent.type(within(sec).getByLabelText("名称"), "x.cli");
    await userEvent.click(within(sec).getByRole("button", { name: "添加" }));
    expect(await within(sec).findByRole("alert")).toHaveTextContent("only be used by the owner");
    expect(within(sec).queryByText("x.cli")).not.toBeInTheDocument();
  });
});

describe("插件声明（flow-command-registration）", () => {
  it("待确认声明展示将要执行的命令；确认前不在清单与命令里，确认后生效", async () => {
    const s = server({ recs: [pending("acme")] });
    mount(s.impl);
    const decls = await screen.findByRole("region", { name: "待批准的接入请求" });
    expect(within(decls).getByText("acme")).toBeInTheDocument();
    expect(within(decls).getAllByText(/\/opt\/acme\/run/)[0]).toBeInTheDocument(); // 摘要里就能看到要运行的命令
    expect(within(section("可以派活的对象")).queryByText("acme.build")).not.toBeInTheDocument();
    expect(within(section("已允许运行的命令")).queryByText("acme")).not.toBeInTheDocument();

    await userEvent.click(within(decls).getByRole("button", { name: "允许它在本机运行这些命令" }));
    expect(await within(section("可以派活的对象")).findByText("acme.build")).toBeInTheDocument();
    expect(await within(section("已允许运行的命令")).findByText("acme")).toBeInTheDocument();
    expect(within(section("待批准的接入请求")).queryByRole("button", { name: "允许它在本机运行这些命令" })).not.toBeInTheDocument();
    const w = s.calls.find((c) => c.path === "/v1/settings/declarations/acme/1/confirm")!;
    expect(w.method).toBe("POST");
    expect(w.csrf).toBe("tok-1");
  });

  it("确认前完整展示声明：access.full、resume、main_session、executors 都可见", async () => {
    mount(server({ recs: [full] }).impl);
    const decls = await screen.findByRole("region", { name: "待批准的接入请求" });
    const text = within(decls).getByTestId("declaration-deep").textContent!;
    for (const v of ["--dangerously-all", "/opt/deep/resume", "--wake-flag", "deep.cli"]) expect(text).toContain(v);
  });

  it("已登记命令同样完整展示声明", async () => {
    mount(server({ recs: [{ ...full, state: "active" }] }).impl);
    const sec = await screen.findByRole("region", { name: "已允许运行的命令" });
    expect(await within(sec).findByTestId("declaration-deep")).toHaveTextContent("--dangerously-all");
  });

  it("拒绝声明 → POST reject；不进入清单与命令", async () => {
    const s = server({ recs: [pending("evil")] });
    mount(s.impl);
    const decls = await screen.findByRole("region", { name: "待批准的接入请求" });
    await userEvent.click(within(decls).getByRole("button", { name: "拒绝" }));
    expect(await within(decls).findByText("没有待批准的请求")).toBeInTheDocument();
    expect(s.calls.some((c) => c.path === "/v1/settings/declarations/evil/1/reject")).toBe(true);
    expect(within(section("已允许运行的命令")).queryByText("evil")).not.toBeInTheDocument();
  });

  it("冲突（409）时显示服务端原因", async () => {
    const r = pending("acme");
    const s = server({ recs: [r] });
    mount(s.impl);
    const decls = await screen.findByRole("region", { name: "待批准的接入请求" });
    r.state = "active"; // 另一处已处理
    await userEvent.click(within(decls).getByRole("button", { name: "允许它在本机运行这些命令" }));
    expect(await within(decls).findByRole("alert")).toHaveTextContent("not pending");
  });

  it("本人直接登记可执行命令：POST /settings/commands，立即出现在命令列表", async () => {
    const s = server();
    mount(s.impl);
    const sec = await screen.findByRole("region", { name: "已允许运行的命令" });
    expect(within(sec).getByText("还没有允许运行的命令")).toBeInTheDocument();
    const text = JSON.stringify(decl("mine", ["/usr/local/bin/mine", "run"]));
    await userEvent.click(within(sec).getByLabelText("声明（JSON）"));
    await userEvent.paste(text);
    await userEvent.click(within(sec).getByRole("button", { name: "登记" }));
    expect(await within(sec).findByText("mine")).toBeInTheDocument();
    expect(within(sec).getAllByText(/\/usr\/local\/bin\/mine/)[0]).toBeInTheDocument();
    const w = s.calls.find((c) => c.method === "POST" && c.path === "/v1/settings/commands")!;
    expect(w.csrf).toBe("tok-1");
    expect(w.body.headless.command).toEqual(["/usr/local/bin/mine", "run"]);
  });

  it("非法 JSON 不发请求；服务端校验失败显示原因且不新增", async () => {
    const s = server();
    mount(s.impl);
    const sec = await screen.findByRole("region", { name: "已允许运行的命令" });
    const box = within(sec).getByLabelText("声明（JSON）");
    await userEvent.click(box);
    await userEvent.paste("{not json");
    await userEvent.click(within(sec).getByRole("button", { name: "登记" }));
    expect(await within(sec).findByRole("alert")).toHaveTextContent("JSON");
    expect(s.calls.some((c) => c.method === "POST")).toBe(false);

    await userEvent.clear(box);
    await userEvent.click(box);
    await userEvent.paste('{"harness":"x","bogus":1}');
    await userEvent.click(within(sec).getByRole("button", { name: "登记" }));
    expect(await within(sec).findByRole("alert")).toHaveTextContent("unknown field");
    expect(within(sec).getByText("还没有允许运行的命令")).toBeInTheDocument();
  });
});
