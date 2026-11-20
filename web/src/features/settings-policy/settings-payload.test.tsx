import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as confirm } from "./index";
import { feature as notify } from "../settings-notify";
import { feature as agents } from "../settings-agents";
import { policyServer } from "./test-server";

// 改版前后 PUT 载荷逐字段一致（设计 §8.1 PR 2）：「策略与人格」拆成「我来拍板 / 提醒与打扰 / 通用」（权限上限的载荷见 settings-agents.test）后，
// 每张卡片「不改直接保存」与「做一次与改版前测试相同的编辑再保存」发出的请求体，都必须与改版前的组件
// （origin/feat/ux-vocab 的 SettingsPolicyPage.tsx）对同一夹具发出的请求体深度相等。期望值按改版前组件的行为写成固定 JSON：
// web_only 规则四个字段都发、空的发空串；disturb 整段带回、routes/daily_cap/mutes 覆盖；其余段只发自己。
const DISTURB = { routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 20, mutes: [] };
const RULE1 = { kind: "payment", tag: "", project: "", category: "funds" };

type Edit = (c: ReturnType<typeof within>) => Promise<void>;
interface Case { name: string; at: string; card: string; edit?: Edit; path: string; body: unknown }

const CASES: Case[] = [
  // 不改直接保存
  { name: "web_only（未改）", at: "/settings", card: "只能我在网页上亲自确认", path: "/v1/settings/policy", body: { web_only: { categories: ["funds", "irreversible"], rules: [RULE1] } } },
  { name: "材料写入（未改）", at: "/settings", card: "Agent 写的文档，哪些先给我过目", path: "/v1/settings/policy", body: { material_write: { default: "proposed", kinds: { minutes: "direct" } } } },
  { name: "打扰策略（未改）", at: "/settings/notify", card: "怎么找我", path: "/v1/settings/policy", body: { disturb: DISTURB } },
  // 对我说话的语言与语气移到设置 › Agent（界面设计 §4），按 §6.1 没有改动时不能保存，所以只比改过之后的载荷。
  {
    name: "Agent 的语言：改选", at: "/settings/agents", card: "对我说话的语言", path: "/v1/settings/policy",
    edit: async (c) => { await userEvent.click(c.getByRole("button", { name: "English" })); },
    body: { persona_lang: "en" },
  },
  // 与改版前测试相同的编辑
  {
    name: "web_only：加类别与一条规则", at: "/settings", card: "只能我在网页上亲自确认", path: "/v1/settings/policy",
    edit: async (c) => {
      await userEvent.click(c.getByLabelText("对外承诺"));
      await userEvent.click(c.getByRole("button", { name: "＋ 再加一条" }));
      await userEvent.type(c.getByLabelText("规则2 标签"), "legal");
      await userEvent.selectOptions(c.getByLabelText("规则2 类别"), "external_commitment");
    },
    body: { web_only: { categories: ["funds", "irreversible", "external_commitment"], rules: [RULE1, { kind: "", tag: "legal", project: "", category: "external_commitment" }] } },
  },
  {
    name: "打扰策略：加一条「类别:优先级」路由", at: "/settings/notify", card: "怎么找我", path: "/v1/settings/policy",
    edit: async (c) => {
      await userEvent.click(c.getByRole("button", { name: "＋ 按优先级单独设" }));
      await userEvent.selectOptions(c.getByLabelText("单独设1 类别"), "reminder");
      await userEvent.selectOptions(c.getByLabelText("单独设1 优先级"), "high");
      await userEvent.selectOptions(c.getByLabelText("单独设1 方式"), "web");
    },
    body: { disturb: { ...DISTURB, routes: { decision: "inject", health: "web", "reminder:high": "web" } } },
  },
  {
    name: "打扰策略：日上限与静音", at: "/settings/notify", card: "怎么找我", path: "/v1/settings/policy",
    edit: async (c) => {
      await userEvent.clear(c.getByLabelText("每天最多推送（条）"));
      await userEvent.type(c.getByLabelText("每天最多推送（条）"), "3");
      await userEvent.click(c.getByRole("button", { name: "＋ 暂停一个" }));
      await userEvent.selectOptions(c.getByLabelText("暂停1 类型"), "project");
      await waitFor(() => expect(within(c.getByLabelText("暂停1 对象")).getByRole("option", { name: "Home" })).toBeInTheDocument());
      await userEvent.selectOptions(c.getByLabelText("暂停1 对象"), "acc-home");
    },
    body: { disturb: { ...DISTURB, daily_cap: 3, mutes: [{ scope: "project", id: "acc-home" }] } },
  },
  {
    name: "材料写入：默认与单个类型", at: "/settings", card: "Agent 写的文档，哪些先给我过目", path: "/v1/settings/policy",
    edit: async (c) => {
      await userEvent.click(within(c.getByRole("group", { name: "其他类型" })).getByRole("button", { name: "直接生效" }));
      await userEvent.click(within(c.getByRole("group", { name: "纪要" })).getByRole("button", { name: "先给我看" }));
    },
    body: { material_write: { default: "direct", kinds: { minutes: "proposed" } } },
  },
  {
    name: "人格：改写", at: "/settings/agents", card: "对我说话的语气", path: "/v1/settings/policy",
    edit: async (c) => {
      const box = c.getByRole("textbox", { name: "对我说话的语气" });
      await userEvent.clear(box);
      await userEvent.type(box, "直接");
    },
    body: { persona: "直接" },
  },
];

beforeEach(() => sessionStorage.clear());

describe("改版前后 PUT 载荷逐字段一致", () => {
  it.each(CASES)("$name", async ({ at, card, edit, path, body }) => {
    const s = policyServer();
    sessionStorage.setItem("apa.csrf", "tok-1");
    render(
      <MemoryRouter initialEntries={[at]}>
        <App features={[confirm, notify, agents]} fetchImpl={s.impl} />
      </MemoryRouter>,
    );
    const c = within(await screen.findByRole("region", { name: card }));
    if (edit) await edit(c);
    await userEvent.click(c.getByRole("button", { name: `保存${card}` }));
    await c.findByRole("status");
    expect(s.puts).toHaveLength(1);
    expect(s.puts[0].path).toBe(path);
    expect(s.puts[0].body).toEqual(body);
    expect(s.puts[0].csrf).toBe("tok-1");
  });
});

describe("设置 › Agent › 对我说话的语气（所有 Agent 共用）", () => {
  it("flow-bg-guards ACT-BGG-08: 超限被服务端拒绝，显示原因且不保存，草稿保留；计数按同一规则估算", async () => {
    const s = policyServer();
    sessionStorage.setItem("apa.csrf", "tok-1");
    render(
      <MemoryRouter initialEntries={["/settings/agents"]}>
        <App features={[agents]} fetchImpl={s.impl} />
      </MemoryRouter>,
    );
    const c = within(await screen.findByRole("region", { name: "对我说话的语气" }));
    const box = c.getByRole("textbox", { name: "对我说话的语气" });
    expect(c.getByText("约 2 / 400")).toBeInTheDocument(); // "Be brief."：8 个非空白 ASCII → (8+3)/4 = 2，同 kernel/policy.EstimateTokens
    await userEvent.clear(box);
    await userEvent.click(box);
    await userEvent.paste("长".repeat(500));
    expect(c.getByText("约 500 / 400")).toBeInTheDocument();
    await userEvent.click(c.getByRole("button", { name: "保存对我说话的语气" }));
    expect(await c.findByRole("alert")).toHaveTextContent("limit 400");
    expect(s.doc.persona).toBe("Be brief.");
    expect(box).toHaveValue("长".repeat(500));
  });

  async function open(persona: string) {
    const s = policyServer();
    s.doc.persona = persona;
    sessionStorage.setItem("apa.csrf", "tok-1");
    render(
      <MemoryRouter initialEntries={["/settings/agents"]}>
        <App features={[agents]} fetchImpl={s.impl} />
      </MemoryRouter>,
    );
    const c = within(await screen.findByRole("region", { name: "对我说话的语气" }));
    const box = await c.findByRole("textbox", { name: "对我说话的语气" });
    await waitFor(() => expect(box).toHaveValue(persona || "Default tone (zh)"));
    return { s, c, box };
  }

  it("没自定义：显示内置默认并标「默认」，没有「恢复默认」；没改动时不能保存（界面设计 §6.1），不写任何东西", async () => {
    const { s, c } = await open("");
    expect(c.getByText("默认")).toBeInTheDocument();
    expect(c.queryByRole("button", { name: "恢复默认" })).toBeNull();
    expect(c.getByRole("button", { name: "保存对我说话的语气" })).toBeDisabled();
    expect(s.puts).toHaveLength(0);
  });

  it("改了就是自定义（「默认」标记换成「恢复默认」）；恢复默认后回到默认并存回空串", async () => {
    const { s, c, box } = await open("");
    await userEvent.type(box, "!");
    expect(c.queryByText("默认")).toBeNull();
    await userEvent.click(c.getByRole("button", { name: "保存对我说话的语气" }));
    await c.findByRole("status");
    expect(s.puts[0].body).toEqual({ persona: "Default tone (zh)!" });
    await userEvent.click(await c.findByRole("button", { name: "恢复默认" }));
    expect(box).toHaveValue("Default tone (zh)");
    expect(c.getByText("默认")).toBeInTheDocument();
    await userEvent.click(c.getByRole("button", { name: "保存对我说话的语气" }));
    await waitFor(() => expect(s.puts).toHaveLength(2));
    expect(s.puts[1].body).toEqual({ persona: "" });
    expect(s.doc.persona).toBe("");
  });

  it("助手语言：只发自己的段；存下后没自定义的默认文字换成那种语言，已改的不动", async () => {
    const { s, c, box } = await open("");
    const l = within(screen.getByRole("region", { name: "对我说话的语言" }));
    await userEvent.click(l.getByRole("button", { name: "English" }));
    await userEvent.click(l.getByRole("button", { name: "保存对我说话的语言" }));
    await waitFor(() => expect(s.puts).toHaveLength(1));
    expect(s.puts[0].body).toEqual({ persona_lang: "en" });
    await waitFor(() => expect(box).toHaveValue("Default tone (en)"));
    await userEvent.type(box, "!");
    await userEvent.click(l.getByRole("button", { name: "中文" }));
    await userEvent.click(l.getByRole("button", { name: "保存对我说话的语言" }));
    await waitFor(() => expect(s.puts).toHaveLength(2));
    expect(s.puts[1].body).toEqual({ persona_lang: "zh" });
    expect(box).toHaveValue("Default tone (en)!");
    expect(c.queryByText("默认")).toBeNull();
  });

  it("已自定义：显示自己的文字、不标「默认」，可恢复默认", async () => {
    const { c, box } = await open("Be brief.");
    expect(c.queryByText("默认")).toBeNull();
    await userEvent.click(c.getByRole("button", { name: "恢复默认" }));
    expect(box).toHaveValue("Default tone (zh)");
  });
});
