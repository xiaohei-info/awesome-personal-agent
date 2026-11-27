import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as notify } from ".";
import { feature as agents } from "../settings-agents";
import { deriveDelivery, policyServer } from "../../app/test-policy-server";

// 改版前后 PUT 载荷逐字段一致（界面设计 §6.1）：「策略与人格」拆成「我来拍板 / 提醒与打扰 / 通用」（权限上限的载荷见 settings-agents.test）后，
// 每张卡片「不改直接保存」与「做一次与改版前测试相同的编辑再保存」发出的请求体，都必须与改版前的组件
// （origin/feat/ux-vocab 的 SettingsPolicyPage.tsx）对同一夹具发出的请求体深度相等。期望值按改版前组件的行为写成固定 JSON：
// disturb 整段带回、routes/daily_cap/mutes 覆盖；其余段只发自己。
const DISTURB = { routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 20, mutes: [] };

type Edit = (c: ReturnType<typeof within>) => Promise<void>;
interface Case { name: string; at: string; card: string; edit?: Edit; path: string; body: unknown }

const CASES: Case[] = [
  // 不改直接保存
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
];

beforeEach(() => sessionStorage.clear());

describe("改版前后 PUT 载荷逐字段一致", () => {
  it.each(CASES)("$name", async ({ at, card, edit, path, body }) => {
    const s = policyServer();
    sessionStorage.setItem("apa.csrf", "tok-1");
    render(
      <MemoryRouter initialEntries={[at]}>
        <App features={[notify, agents]} fetchImpl={withSession(s.impl)} />
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
