import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { policyServer } from "../settings-policy/test-server";

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/access"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const card = async (name: string) => within(await screen.findByRole("region", { name }));
const save = async (name: string) => userEvent.click((await card(name)).getByRole("button", { name: `保存${name}` }));
const CAP = "后台运行能做多少";
const LISTS = "信任名单";

beforeEach(() => sessionStorage.clear());

describe("设置 › 后台权限", () => {
  it("三档用名字和能做什么的清单显示，默认档选中；按宿主单独设只列该宿主声明过的档", async () => {
    const s = policyServer();
    mount(s.impl);
    const c = await card(CAP);
    expect(c.getByRole("radio", { name: "只看" })).toBeChecked();
    expect(c.getByRole("radio", { name: "可改文件" })).not.toBeChecked();
    expect(c.getAllByText(/改工作目录里的文件/).length).toBeGreaterThan(0);
    expect(screen.getByRole("main")).not.toHaveTextContent(/\bread\b|\bedit\b|harness/);
    await userEvent.click(c.getByRole("button", { name: "＋ 按宿主单独设" }));
    await waitFor(() => expect(c.getByLabelText("宿主 1")).toHaveValue("hermes"));
    expect(within(c.getByLabelText("宿主1 档位")).getAllByRole("option").map((o) => o.getAttribute("value"))).toEqual(["read", "edit"]);
    expect(c.getByText("提供：只看、可改文件")).toBeInTheDocument();
    await userEvent.selectOptions(c.getByLabelText("宿主1 档位"), "edit");
    await save(CAP);
    await c.findByRole("status");
    expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/access-limits", body: { permission_cap: { default: "read", by_harness: { hermes: "edit" } } } });
  });

  it("flow-bg-guards ACT-BGG-08: 两份白名单分开显示，一起保存；发送者、回答人用标签增删", async () => {
    const s = policyServer();
    mount(s.impl);
    const a = await card(LISTS);
    expect(a.getByLabelText("跟进来源 1")).toHaveValue("telegram"); // 不在来源与渠道列表里的已有键保留
    await userEvent.type(a.getByRole("textbox", { name: "跟进来源1 发送者" }), "bob{Enter}");
    await userEvent.click(a.getByRole("button", { name: "＋ 加一个渠道" }));
    await waitFor(() => expect(a.getByLabelText("渠道 1")).toHaveValue("slack"));
    await userEvent.type(a.getByRole("textbox", { name: "渠道1 回答人" }), "carol{Enter}");
    await userEvent.click(a.getByRole("button", { name: "去掉 alice" }));
    await save(LISTS);
    await a.findByRole("status");
    expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/allowlists", body: { forward_allowlist: { telegram: ["bob"] }, channel_answerers: { slack: ["carol"] } } });
  });

  it("同一个来源列两次时在本地提示，不发请求", async () => {
    const s = policyServer();
    mount(s.impl);
    const a = await card(LISTS);
    await userEvent.click(a.getByRole("button", { name: "＋ 加一个来源" }));
    await userEvent.click(a.getByRole("button", { name: "＋ 加一个来源" }));
    await waitFor(() => expect(within(a.getByLabelText("跟进来源 2")).getByRole("option", { name: "mail" })).toBeInTheDocument());
    await userEvent.selectOptions(a.getByLabelText("跟进来源 2"), "mail");
    await userEvent.selectOptions(a.getByLabelText("跟进来源 3"), "mail");
    await save(LISTS);
    expect(await a.findByRole("alert")).toHaveTextContent("重复");
    expect(s.puts).toHaveLength(0);
  });
});
