import { withSession } from "../../app/test-session";
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
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("设置 › 权限总览（原「策略与人格」的地址 /settings）", () => {
  it("每个 Agent 一列、十组一行，读判定的那份；只看，名字链到它自己的页面；写明只有本人能做的事", async () => {
    const s = policyServer({ agents: [
      { id: "agent_1", harness: "hermes", name: "日常", enabled: true, rev: 1, permission: { groups: { view: {}, organize: {}, source_events: {}, decide: {}, purge: { until: "2099-01-01T00:00:00Z" } }, tool: "edit" } },
      { id: "legacy:hermes", harness: "hermes", name: "", enabled: true, rev: 0, permission: { groups: { view: {}, organize: {}, source_events: {} }, tool: "read" } },
    ] });
    mount(s.impl);
    expect(await screen.findByRole("link", { name: "日常" })).toHaveAttribute("href", "/settings/agents/agent_1#permissions");
    const decide = screen.getByRole("row", { name: /替我拍板/ });
    expect(within(decide).getAllByRole("cell").map((c) => c.textContent)).toEqual(["允许", "不允许"]);
    expect(within(screen.getByRole("row", { name: /物理删除/ })).getAllByRole("cell")[0]).toHaveTextContent("允许到");
    expect(screen.queryByRole("radio")).toBeNull();
    expect(screen.getByText(/只有你本人能做/)).toBeInTheDocument();
  });

  it("flow-web-only: 非 Web 会话读取被拒绝时显示原因并可重试", async () => {
    mount(policyServer({ webOnlyDenied: true }).impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("only be used by the owner in the Web UI");
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});
