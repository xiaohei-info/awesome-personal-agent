import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { ApiError, type ApiClient } from "./api";
import { ErrorNote } from "./errors";
import { errorText, setChoice } from "./i18n";
import { forgetNames, loadNames } from "./names";
import { setRawShown } from "./raw";
import { Pick, Val } from "./values";
import { vocab, type Vocab } from "./vocab";
import { SettingsGeneralPage } from "../features/settings-general/SettingsGeneralPage";

describe("取值词表", () => {
  it("封闭集合缺一个 ID 的显示名时编译失败；中英两份的键一致", () => {
    // @ts-expect-error 少了 dropped：ids.ts 的 PHASE 有而词表没写，tsc 报错
    const missing: Vocab["phase"] = { proposed: { label: "a" }, open: { label: "b" }, waiting: { label: "c" }, needs_input: { label: "d" }, done: { label: "e" } };
    expect(Object.keys(missing)).toHaveLength(5);
    for (const k of Object.keys(vocab.zh) as (keyof Vocab)[]) {
      if (typeof vocab.zh[k] === "object") expect(Object.keys(vocab.en[k]).sort(), k).toEqual(Object.keys(vocab.zh[k]).sort());
    }
  });

  it("Val 显示名字；打开「显示原始值」后带上 ID；查不到的 ID 原样显示；渠道路由显示渠道名", () => {
    const { container, rerender } = render(<p><Val k="phase" id="needs_input" /></p>);
    expect(container).toHaveTextContent(/^等你决定$/);
    act(() => setRawShown(true));
    expect(container).toHaveTextContent("等你决定needs_input");
    act(() => setRawShown(false));
    rerender(<p><Val k="materialKind" id="pack-made-kind" /></p>);
    expect(container).toHaveTextContent("pack-made-kind");
    rerender(<p><Val k="route" id="channel:chat" /></p>);
    expect(container).toHaveTextContent("推送到 chat");
    act(() => setChoice("en"));
    rerender(<p><Val k="phase" id="needs_input" /></p>);
    expect(container).toHaveTextContent("Needs you");
  });

  it("Pick：选项文字是名字，提交的是 ID", async () => {
    let got = "";
    render(<Pick k="perm" label="档位" value="read" onChange={(v) => (got = v)} />);
    const sel = screen.getByLabelText("档位");
    expect(within(sel).getAllByRole("option").map((o) => [o.getAttribute("value"), o.textContent])).toEqual([
      ["read", "只看"], ["edit", "可改文件"], ["full", "完全放开"],
    ]);
    await userEvent.selectOptions(sel, "edit");
    expect(got).toBe("edit");
  });
});

describe("按错误码显示错误", () => {
  const e = (code: string, why = "only from a Web session", next = "open the web") => new ApiError(403, { code, why, next });

  it("已知码：当前语言的一句话加服务端原因；未知码：原样显示 why 与 next", () => {
    expect(errorText(e("web_only"))).toBe("这件事需要你在网页上亲自确认（only from a Web session）");
    expect(errorText(e("future_code"))).toBe("only from a Web session（open the web）");
    act(() => setChoice("en"));
    expect(errorText(e("not_found", "no such item"))).toBe("Record not found (no such item)");
  });

  it("ErrorNote：说得清的码主行只有那句话，原因收进详情；只说了类别的码主行带上原因", () => {
    const { rerender } = render(<ErrorNote error={e("web_only")} />);
    const alert = screen.getByRole("alert");
    expect(alert.querySelector("p")).toHaveTextContent(/^这件事需要你在网页上亲自确认$/);
    expect(within(alert).getByText("详情")).toBeInTheDocument();
    expect(alert).toHaveTextContent("only from a Web session（open the web）");
    rerender(<ErrorNote error={e("invalid_request", "title is empty")} />);
    expect(screen.getByRole("alert").querySelector("p")).toHaveTextContent("请求有误（title is empty）");
    rerender(<ErrorNote error={new Error("network down")} />);
    expect(screen.getByRole("alert")).toHaveTextContent("network down");
    expect(screen.queryByText("详情")).not.toBeInTheDocument();
  });
});

describe("id → 显示名", () => {
  it("同一个客户端只取一次；取不到时下次重试；forgetNames 让下次重取", async () => {
    const calls: string[] = [];
    let fail = true;
    const api = {
      get: async (p: string) => {
        calls.push(p);
        if (fail) throw new Error("down");
        return { projects: [{ id: "prj_1", name: "家务" }, { id: "prj_2" }] };
      },
    } as unknown as ApiClient;
    await expect(loadNames(api, "project")).rejects.toThrow("down");
    fail = false;
    expect(await loadNames(api, "project")).toEqual([{ id: "prj_1", name: "家务" }, { id: "prj_2", name: "prj_2" }]);
    await loadNames(api, "project");
    expect(calls).toEqual(["/projects", "/projects"]);
    forgetNames(api, "project");
    await loadNames(api, "project");
    expect(calls).toHaveLength(3);
  });
});

describe("设置 › 通用 › 显示原始值", () => {
  it("默认关；打开后各处带上 ID，存在这台浏览器", async () => {
    render(<><SettingsGeneralPage /><p data-testid="v"><Val k="perm" id="read" /></p></>);
    const box = screen.getByRole("checkbox", { name: "显示原始值" });
    expect(box).not.toBeChecked();
    expect(screen.getByTestId("v")).toHaveTextContent(/^只看$/);
    await userEvent.click(box);
    expect(localStorage.getItem("apa.raw")).toBe("1");
    expect(screen.getByTestId("v")).toHaveTextContent("只看read");
  });
});
