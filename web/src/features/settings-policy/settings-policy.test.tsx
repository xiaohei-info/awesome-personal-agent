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

beforeEach(() => sessionStorage.clear());

describe("设置 › 我来拍板（原「策略与人格」的地址 /settings）", () => {
  it("每个 Agent 一张档位卡，读判定的那份（旧身份也按实际值）；就地改、只调一次档位路由；硬线写明只有本人能做", async () => {
    const s = policyServer({ agents: [
      { id: "agent_1", harness: "hermes", name: "日常", enabled: true, rev: 1, tier: "decide", tier_projects: [] },
      { id: "legacy:hermes", harness: "hermes", name: "", enabled: true, rev: 0, tier: "rules" },
    ] });
    mount(s.impl);
    const daily = within(await screen.findByRole("region", { name: "日常" }));
    expect(daily.getByRole("radio", { name: /也替我拍板/ })).toBeChecked();
    const legacy = within(screen.getByRole("region", { name: "hermes" }));
    expect(legacy.getByRole("radio", { name: /连日常规矩也替我改/ })).toBeChecked();
    expect(screen.queryByRole("link", { name: "去改" })).toBeNull();
    await userEvent.click(legacy.getByRole("radio", { name: /只整理/ }));
    await userEvent.click(legacy.getByRole("button", { name: "保存hermes" }));
    await waitFor(() => expect(s.tiers).toHaveLength(1));
    expect(s.tiers[0]).toMatchObject({ url: "/v1/settings/agents/legacy%3Ahermes/tier", body: { tier: "organize", projects: [], version: 1 } });
    expect(screen.getByText("只有你本人能做")).toBeInTheDocument();
  });

  it("flow-web-only: 非 Web 会话读取被拒绝时显示原因并可重试", async () => {
    mount(policyServer({ webOnlyDenied: true }).impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("only be used by the owner in the Web UI");
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});
