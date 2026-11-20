import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

beforeEach(() => sessionStorage.clear());

describe("旧的一次性表单链接", () => {
  it("只做导航：跳到要登录的页面，不读令牌、不发请求", async () => {
    const urls: string[] = [];
    const f = (async (u: string) => (urls.push(u), new Response("{}", { status: 404 }))) as unknown as typeof fetch;
    render(
      <MemoryRouter initialEntries={["/forms/tok_old"]}>
        <App features={[feature]} fetchImpl={f} />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("heading", { name: "登录" })).toBeInTheDocument();
    expect(urls.some((u) => u.includes("tok_old"))).toBe(false);
  });
});
