import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { policyServer } from "./test-server";

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const card = async (name: string) => within(await screen.findByRole("region", { name }));
const save = async (name: string) => userEvent.click((await card(name)).getByRole("button", { name: `保存${name}` }));
const WEB_ONLY = "只能我在网页上亲自确认";
const DOCS = "agent 写的文档，哪些先给我过目";

beforeEach(() => sessionStorage.clear());

describe("设置 › 我来拍板（原「策略与人格」的地址 /settings）", () => {
  it("flow-bg-guards ACT-BGG-08: 载入显示当前设置；类别显示中文名，不露内部 ID", async () => {
    mount(policyServer().impl);
    const w = await card(WEB_ONLY);
    expect(w.getByLabelText("资金")).toBeChecked();
    expect(w.getByLabelText("不可逆")).toBeChecked();
    expect(w.getByLabelText("对外承诺")).not.toBeChecked();
    expect(screen.getByRole("main")).not.toHaveTextContent(/funds|external_commitment|direct|proposed/);
    expect(w.getByLabelText("规则1 类型")).toHaveValue("payment");
    expect(w.getByLabelText("规则1 类别")).toHaveValue("funds");
    const d = await card(DOCS);
    expect(within(d.getByRole("group", { name: "纪要" })).getByRole("button", { name: "直接生效" })).toHaveAttribute("aria-pressed", "true");
    // 没登记的类型取「其他类型」（default = proposed）
    expect(within(d.getByRole("group", { name: "需求" })).getByRole("button", { name: "先给我看" })).toHaveAttribute("aria-pressed", "true");
    // 规范的写入策略在内核里恒为先确认，这一行没有开关
    expect(d.queryByRole("group", { name: "规范" })).toBeNull();
    expect(d.getByText(/始终先给我看/)).toBeInTheDocument();
    // 打扰、权限、人格不在这一页
    expect(screen.queryByRole("region", { name: "怎么找我" })).toBeNull();
  });

  it("flow-bg-guards ACT-BGG-08: 编辑类别与规则，服务端拒绝未登记类别；规则的类别只能从已选类别里选", async () => {
    const s = policyServer();
    mount(s.impl);
    const c = await card(WEB_ONLY);
    await userEvent.click(c.getByRole("button", { name: "＋ 再加一条" }));
    await userEvent.type(c.getByLabelText("规则2 标签"), "legal");
    const cat = c.getByLabelText("规则2 类别");
    expect(within(cat).getAllByRole("option").map((o) => o.getAttribute("value"))).toEqual(["funds", "irreversible"]);
    await userEvent.click(c.getByLabelText("对外承诺"));
    await userEvent.selectOptions(c.getByLabelText("规则2 类别"), "external_commitment");
    await save(WEB_ONLY);
    expect(await c.findByRole("status")).toHaveTextContent("已保存");
    expect(s.doc.web_only.rules[1]).toMatchObject({ tag: "legal", category: "external_commitment" });
    await waitFor(() => expect(c.getByLabelText("对外承诺")).toBeChecked());
    await userEvent.click(c.getByRole("button", { name: "删除规则 2" }));
    await save(WEB_ONLY);
    await c.findByRole("status");
    expect(s.doc.web_only.rules).toHaveLength(1);
    // 取消勾选仍被规则用着的类别：服务端拒绝，什么都没保存
    await userEvent.click(c.getByLabelText("资金"));
    await save(WEB_ONLY);
    expect(await c.findByRole("alert")).toHaveTextContent("not in categories");
    expect(s.doc.web_only.categories).toContain("funds");
  });

  it("自定义类别：加进来后可勾选、可被规则引用；显示原样的名字", async () => {
    const s = policyServer();
    mount(s.impl);
    const c = await card(WEB_ONLY);
    await userEvent.type(c.getByLabelText("自定义类别"), "legal_hold{Enter}");
    expect(c.getByLabelText("legal_hold")).toBeChecked();
    await save(WEB_ONLY);
    await c.findByRole("status");
    expect(s.doc.web_only.categories).toEqual(["funds", "irreversible", "legal_hold"]);
  });

  it("材料写入：分段按钮改单个类型或其他类型，只发 material_write 一段", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await card(DOCS);
    await userEvent.click(within(d.getByRole("group", { name: "方案" })).getByRole("button", { name: "直接生效" }));
    await save(DOCS);
    await d.findByRole("status");
    expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/policy", body: { material_write: { default: "proposed", kinds: { minutes: "direct", design: "direct" } } }, csrf: "tok-1" });
    expect(Object.keys(s.puts.at(-1)!.body)).toEqual(["material_write"]);
  });

  it("flow-web-only: 非 Web 会话读取被拒绝时显示 web_only 原因并可重试", async () => {
    mount(policyServer({ webOnlyDenied: true }).impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("only be used by the owner in the Web UI");
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});
