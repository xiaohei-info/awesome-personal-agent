import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as notify } from "../settings-notify";
import { feature as agents } from "../settings-agents";
import { deriveDelivery, policyServer } from "./test-server";

// 改版前后 PUT 载荷逐字段一致（界面设计 §6.1）：「策略与人格」拆成「我来拍板 / 提醒与打扰 / 通用」（权限上限的载荷见 settings-agents.test）后，
// 每张卡片「不改直接保存」与「做一次与改版前测试相同的编辑再保存」发出的请求体，都必须与改版前的组件
// （origin/feat/ux-vocab 的 SettingsPolicyPage.tsx）对同一夹具发出的请求体深度相等。期望值按改版前组件的行为写成固定 JSON：
// disturb 整段带回、routes/daily_cap/mutes 覆盖；其余段只发自己。
const DISTURB = { routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 20, mutes: [] };

type Edit = (c: ReturnType<typeof within>) => Promise<void>;
interface Case { name: string; at: string; card: string; edit?: Edit; path: string; body: unknown }

const CASES: Case[] = [
  // 不改直接保存
  // 对我说话的语言与语气移到设置 › Agent（界面设计 §4），按 §6.1 没有改动时不能保存，所以只比改过之后的载荷。
  {
    name: "Agent 的语言：改选", at: "/settings/agents", card: "对我说话的语言", path: "/v1/settings/policy",
    edit: async (c) => { await userEvent.click(c.getByRole("button", { name: "English" })); },
    body: { persona_lang: "en" },
  },
  // 与改版前测试相同的编辑
  // 设计 v2：各类提醒的去处搬到各自的页面（单选，见各页测试）；设置页只剩全局项。默认渠道一节发整段 disturb，矩阵只换 default_channel。
  {
    name: "默认渠道：改选，只换 default_channel，各行去处与冻结的旧键原样带回", at: "/settings/notify", card: "默认渠道", path: "/v1/settings/policy",
    edit: async (c) => {
      const pick = c.getByLabelText("默认渠道");
      await waitFor(() => expect(within(pick).getByRole("option", { name: "slack" })).toBeInTheDocument());
      await userEvent.selectOptions(pick, "channel:slack");
    },
    body: { disturb: { ...DISTURB, delivery: { default_channel: "channel:slack", rows: deriveDelivery(DISTURB).rows } } },
  },
  // 提醒与推送拆成各自保存的小节（界面设计 §6.1）：每节发整段 disturb，只换自己改过的字段。
  {
    name: "打扰策略：日上限", at: "/settings/notify", card: "每天最多推几条", path: "/v1/settings/policy",
    edit: async (c) => {
      await userEvent.clear(c.getByLabelText("每天最多推（条）"));
      await userEvent.type(c.getByLabelText("每天最多推（条）"), "3");
    },
    body: { disturb: { ...DISTURB, daily_cap: 3 } },
  },
  {
    name: "打扰策略：暂停名单", at: "/settings/notify", card: "暂停名单", path: "/v1/settings/policy",
    edit: async (c) => {
      await userEvent.click(c.getByRole("button", { name: "＋ 暂停一个" }));
      await userEvent.selectOptions(c.getByLabelText("暂停1 类型"), "project");
      await waitFor(() => expect(within(c.getByLabelText("暂停1 对象")).getByRole("option", { name: "Home" })).toBeInTheDocument());
      await userEvent.selectOptions(c.getByLabelText("暂停1 对象"), "acc-home");
    },
    body: { disturb: { ...DISTURB, mutes: [{ scope: "project", id: "acc-home" }] } },
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
        <App features={[notify, agents]} fetchImpl={s.impl} />
      </MemoryRouter>,
    );
    const c = within(await screen.findByRole("region", { name: card }));
    if (edit) await edit(c);
    await userEvent.click(c.getByRole("button", { name: `保存${card}` }));
    await c.findByText("已保存"); // 矩阵卡里嵌着到点提醒的预览（role=status），按保存结果的文字找
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
