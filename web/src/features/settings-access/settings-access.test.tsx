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
const HOST = "没指定时用哪个宿主";

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

  it("默认宿主：没设时提示后果；只列已确认声明的宿主；保存只发 default_harness 这一段，其余策略不动", async () => {
    const s = policyServer();
    mount(s.impl);
    const c = await card(HOST);
    expect(c.getByText("不设时后台任务会停下来问你")).toBeInTheDocument();
    const pick = c.getByRole("combobox", { name: HOST });
    expect(pick).toHaveValue("");
    await waitFor(() => expect(within(pick).getAllByRole("option").map((o) => o.getAttribute("value"))).toEqual(["", "hermes"]));
    await userEvent.selectOptions(pick, "hermes");
    await save(HOST);
    await c.findByRole("status");
    expect(s.puts.at(-1)).toEqual({ path: "/v1/settings/policy", body: { default_harness: "hermes" }, csrf: "tok-1" });
    expect(s.doc.persona).toBe("Be brief."); // 别的段原样
    expect(c.queryByText("不设时后台任务会停下来问你")).not.toBeInTheDocument();
  });
});
