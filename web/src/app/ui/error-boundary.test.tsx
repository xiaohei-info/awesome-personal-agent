import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ErrorBoundary } from "./ErrorBoundary";

// 错误边界：子组件渲染抛错时只换掉这一块，其余照常；重试与换页（resetKey）都能恢复；错误连同组件堆栈写 console.error。
let broken = true;
function Flaky() {
  if (broken) throw new Error("boom");
  return <p>卡片内容</p>;
}

describe("ErrorBoundary", () => {
  afterEach(() => vi.restoreAllMocks());

  it("一块出错时页面其余部分照常，提示不露堆栈；重试后恢复", async () => {
    broken = true;
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <div>
        <h1>页面标题</h1>
        <ErrorBoundary><Flaky /></ErrorBoundary>
        <p>另一块</p>
      </div>,
    );
    expect(screen.getByRole("heading", { name: "页面标题" })).toBeInTheDocument();
    expect(screen.getByText("另一块")).toBeInTheDocument();
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("这一块没能显示出来");
    expect(alert).not.toHaveTextContent("boom");
    expect(log).toHaveBeenCalledWith("render failed", expect.any(Error), expect.stringContaining("Flaky"));

    broken = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(screen.getByText("卡片内容")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("路由级：内容区换成页面提示；地址变了（resetKey）自动重置", () => {
    broken = true;
    vi.spyOn(console, "error").mockImplementation(() => {});
    const { rerender } = render(<><nav>导航</nav><ErrorBoundary page resetKey="/a"><Flaky /></ErrorBoundary></>);
    expect(screen.getByText("导航")).toBeInTheDocument();
    expect(screen.getByRole("main")).toHaveTextContent("这一页没能显示出来");

    broken = false;
    rerender(<><nav>导航</nav><ErrorBoundary page resetKey="/b"><Flaky /></ErrorBoundary></>);
    expect(screen.getByText("卡片内容")).toBeInTheDocument();
  });
});
