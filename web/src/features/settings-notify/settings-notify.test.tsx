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
    <MemoryRouter initialEntries={["/settings/notify"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const CARD = "怎么找我";
const card = async () => within(await screen.findByRole("region", { name: CARD }));
const save = async () => userEvent.click((await card()).getByRole("button", { name: `保存${CARD}` }));
const options = (el: HTMLElement) => within(el).getAllByRole("option").map((o) => o.getAttribute("value"));

beforeEach(() => sessionStorage.clear());

describe("设置 › 提醒与打扰", () => {
  it("路由只能选合法值：下次聊天时告诉我、只放网页未读、已接入的渠道；没写的类别显示实际生效的「只放网页未读」", async () => {
    mount(policyServer().impl);
    const d = await card();
    const decision = d.getByLabelText("决策");
    expect(decision).toHaveValue("inject");
    await waitFor(() => expect(options(decision)).toEqual(["inject", "web", "channel:slack"])); // 吊销了的渠道不列
    expect(within(decision).getByRole("option", { name: "下次聊天时告诉我" })).toBeInTheDocument();
    expect(d.getByLabelText("简报")).toHaveValue("web"); // 文档里没有 brief：内核退回 Web 未读（kernel/attention.route）
    expect(screen.getByRole("main")).not.toHaveTextContent(/inject|channel:/);
  });

  it("内置渠道按名字列出；指向已停用或已删除连接的路由照样显示并标明，主视图不出现连接 id", async () => {
    const s = policyServer({
      connections: [{ id: "ch-1a2b", name: "我的 iPhone", enabled: true }, { id: "ch-off0", name: "旧手机", enabled: false }],
      routes: { decision: "channel:ch-1a2b", brief: "channel:ch-off0", reminder: "channel:ch-gone", health: "web" },
    });
    mount(s.impl);
    const d = await card();
    const decision = d.getByLabelText("决策");
    await waitFor(() => expect(options(decision)).toEqual(["inject", "web", "channel:ch-1a2b", "channel:slack"]));
    expect(within(decision).getByRole("option", { name: "推送到 我的 iPhone" })).toBeInTheDocument();
    expect(within(d.getByLabelText("简报")).getByRole("option", { name: "推送到 旧手机（已停用，先放网页未读）" })).toBeInTheDocument();
    expect(within(d.getByLabelText("提醒")).getByRole("option", { name: "推送到已删除的渠道（先放网页未读）" })).toBeInTheDocument();
    expect(screen.getByRole("main")).not.toHaveTextContent(/ch-|channel:/);
    await userEvent.selectOptions(d.getByLabelText("简报"), "channel:ch-1a2b");
    await save();
    await d.findByRole("status");
    expect(s.puts.at(-1)!.body.disturb.routes).toEqual({ decision: "channel:ch-1a2b", brief: "channel:ch-1a2b", reminder: "channel:ch-gone", health: "web" });
  });

  it("指向已删除的自写程序渠道的路由照样显示并标明；还在的渠道按名字显示", async () => {
    const s = policyServer({ routes: { decision: "channel:gone1", reminder: "channel:slack", brief: "web", health: "web" } });
    mount(s.impl);
    const d = await card();
    const decision = d.getByLabelText("决策");
    await waitFor(() => expect(options(decision)).toEqual(["inject", "web", "channel:slack", "channel:gone1"]));
    expect(within(decision).getByRole("option", { name: "推送到已删除的渠道（先放网页未读）" })).toBeInTheDocument();
    expect(within(d.getByLabelText("提醒")).getByRole("option", { name: "推送到 slack" })).toBeInTheDocument();
  });

  it("ACT-DLV-16/24: 在 Web 设置日上限与暂停提醒并保存；今天已推送的条数显示在上限下面", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await card();
    expect(await d.findByText("今天已推送 7 条 · 超出的留在未读")).toBeInTheDocument();
    await userEvent.clear(d.getByLabelText("每天最多推送（条）"));
    await userEvent.type(d.getByLabelText("每天最多推送（条）"), "3");
    await userEvent.click(d.getByRole("button", { name: "＋ 暂停一个" }));
    await userEvent.selectOptions(d.getByLabelText("暂停1 类型"), "project");
    await waitFor(() => expect(options(d.getByLabelText("暂停1 对象"))).toContain("acc-home"));
    expect(within(d.getByLabelText("暂停1 对象")).getByRole("option", { name: "Home" })).toBeInTheDocument();
    await userEvent.selectOptions(d.getByLabelText("暂停1 对象"), "acc-home");
    await save();
    await d.findByRole("status");
    expect(s.puts.at(-1)!.body.disturb).toMatchObject({ daily_cap: 3, mutes: [{ scope: "project", id: "acc-home" }], merge_window_sec: 1800 });

    await userEvent.type(d.getByLabelText("暂停1 到期"), "2030-01-01T10:00");
    await save();
    await d.findByRole("status");
    expect(s.doc.disturb.mutes[0].until).toBe(new Date("2030-01-01T10:00").toISOString());

    await userEvent.click(d.getByRole("button", { name: "恢复 1" }));
    await userEvent.click(d.getByLabelText("不限"));
    await save();
    await d.findByRole("status");
    expect(s.doc.disturb).toMatchObject({ daily_cap: 0, mutes: [] });
  });

  it("暂停提醒的来源下拉按名字列出（本人反馈），选中后提交 id；不出现 src_ 原始 id", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await card();
    await userEvent.click(d.getByRole("button", { name: "＋ 暂停一个" }));
    await userEvent.selectOptions(d.getByLabelText("暂停1 类型"), "source");
    const pick = d.getByLabelText("暂停1 对象");
    await waitFor(() => expect(within(pick).getByRole("option", { name: "Apple Calendar" })).toHaveValue("src_cal_apple"));
    expect(within(pick).getAllByRole("option").map((o) => o.textContent)).toEqual(["", "Apple Calendar", "QQ Mail"]);
    await userEvent.selectOptions(pick, "src_mail_qq");
    await save();
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb.mutes).toEqual([{ scope: "source", id: "src_mail_qq" }]));
  });

  it("日上限不是非负整数时在本地提示，不发请求", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await card();
    await userEvent.clear(d.getByLabelText("每天最多推送（条）"));
    await userEvent.type(d.getByLabelText("每天最多推送（条）"), "-2");
    await save();
    expect(await d.findByRole("alert")).toHaveTextContent("每天最多推送");
    expect(s.puts).toHaveLength(0);
  });

  it("按优先级单独设：同一类别与优先级设两次时在本地提示，不发请求", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await card();
    await userEvent.click(d.getByRole("button", { name: "＋ 按优先级单独设" }));
    await userEvent.click(d.getByRole("button", { name: "＋ 按优先级单独设" }));
    await save();
    expect(await d.findByRole("alert")).toHaveTextContent("重复");
    expect(s.puts).toHaveLength(0);
  });

  it("安静时段与更多里的开关：只发改过的字段，其余原样带回；关掉安静时段就去掉 quiet", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await card();
    await userEvent.click(d.getByLabelText("开启安静时段"));
    await userEvent.click(d.getByLabelText("截止后还没处理，再提醒一次"));
    await userEvent.selectOptions(d.getByLabelText("低优先级提醒合并"), "15");
    await save();
    await d.findByRole("status");
    const body = s.puts.at(-1)!.body.disturb;
    expect(body.quiet).toMatchObject({ start: "22:00", end: "08:00" });
    expect(typeof body.quiet.zone).toBe("string");
    expect(body).toMatchObject({ remind_on_deadline: true, merge_window_sec: 900, quiet_exempt: true, routes: { decision: "inject", health: "web" } });
    expect(body).not.toHaveProperty("threshold_judge");
    await userEvent.click(d.getByLabelText("开启安静时段"));
    await save();
    await d.findByRole("status");
    expect(s.puts.at(-1)!.body.disturb).not.toHaveProperty("quiet");
  });

  it("唤醒 Agent只在高级里出现，选项里多一个「唤醒 Agent 开工」", async () => {
    mount(policyServer().impl);
    const d = await card();
    expect(within(d.getByLabelText("决策")).queryByRole("option", { name: "唤醒 Agent 开工" })).toBeNull();
    const wake = d.getByLabelText("唤醒 Agent");
    expect(within(wake).getByRole("option", { name: "唤醒 Agent 开工" })).toBeInTheDocument();
  });
});
