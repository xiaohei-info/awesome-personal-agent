import { render, screen, within } from "@testing-library/react";
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
  it("每个 Agent 显示判定读的那份档位；旧设置迁移来的恒为只整理；硬线写明只有本人能做", async () => {
    mount(policyServer({ agents: [
      { id: "agent_1", harness: "hermes", name: "日常", enabled: true, rev: 1, tier: "decide", tier_projects: [] },
      { id: "legacy:hermes", harness: "hermes", name: "", enabled: true, rev: 0, tier: "rules" },
    ] }).impl);
    const list = within(await screen.findByRole("region", { name: "每个 Agent 现在的档位" }));
    expect(within(list.getByRole("listitem", { name: "日常" })).getByText("也替我拍板")).toBeInTheDocument();
    expect(within(list.getByRole("listitem", { name: "hermes" })).getByText("只整理")).toBeInTheDocument();
    expect(within(list.getByRole("listitem", { name: "日常" })).getByRole("link", { name: "去改" })).toHaveAttribute("href", "/settings/agents/agent_1#tier-agent_1");
    expect(screen.getByText("只有你本人能做")).toBeInTheDocument();
  });

  it("flow-web-only: 非 Web 会话读取被拒绝时显示原因并可重试", async () => {
    mount(policyServer({ webOnlyDenied: true }).impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("only be used by the owner in the Web UI");
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});
