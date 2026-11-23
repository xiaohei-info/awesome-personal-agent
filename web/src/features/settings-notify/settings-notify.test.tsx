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
const CARD = "各类提醒怎么送";
const card = async () => within(await screen.findByRole("region", { name: CARD }));
const save = async () => userEvent.click((await card()).getByRole("button", { name: `保存${CARD}` }));
// 每一节单独保存（界面设计 §6.1）：保存发的是整段 disturb，已保存的原样带回，只换这一节改过的字段。
const section = async (name: string) => within(await screen.findByRole("region", { name }));
const saveIn = async (name: string) => userEvent.click((await section(name)).getByRole("button", { name: `保存${name}` }));
const options = (el: HTMLElement) => within(el).getAllByRole("option").map((o) => o.getAttribute("value"));

beforeEach(() => sessionStorage.clear());

describe("设置 › 提醒与打扰", () => {
  it("路由只能选合法值：下次聊天时告诉我、只放在今天页的提醒里、已接入的渠道；没写的类别显示实际生效的「只放在今天页的提醒里」", async () => {
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
    expect(within(decision).getByRole("option", { name: "发到 我的 iPhone" })).toBeInTheDocument();
    expect(within(d.getByLabelText("简报")).getByRole("option", { name: "发到 旧手机（已停用，先放在今天页）" })).toBeInTheDocument();
    expect(within(d.getByLabelText("提醒")).getByRole("option", { name: "发到已删除的渠道（先放在今天页）" })).toBeInTheDocument();
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
    expect(within(decision).getByRole("option", { name: "发到已删除的渠道（先放在今天页）" })).toBeInTheDocument();
    expect(within(d.getByLabelText("提醒")).getByRole("option", { name: "发到 slack" })).toBeInTheDocument();
  });

  it("ACT-DLV-16/24: 在 Web 设置日上限与暂停提醒，各自保存；今天已发的条数显示在上限下面；没改动时保存不可点", async () => {
    const s = policyServer();
    mount(s.impl);
    const cap = await section("每天最多发几条");
    expect(await cap.findByText("今天已发 7 条 · 超出的留在今天页的提醒里")).toBeInTheDocument();
    expect(cap.getByRole("button", { name: "保存每天最多发几条" })).toBeDisabled();
    await userEvent.clear(cap.getByLabelText("每天最多发（条）"));
    await userEvent.type(cap.getByLabelText("每天最多发（条）"), "3");
    expect(cap.getByText("未保存")).toBeInTheDocument();
    const m = await section("暂停提醒");
    await userEvent.click(m.getByRole("button", { name: "＋ 暂停一个" }));
    await userEvent.selectOptions(m.getByLabelText("暂停1 类型"), "project");
    await waitFor(() => expect(options(m.getByLabelText("暂停1 对象"))).toContain("acc-home"));
    expect(within(m.getByLabelText("暂停1 对象")).getByRole("option", { name: "Home" })).toBeInTheDocument();
    await userEvent.selectOptions(m.getByLabelText("暂停1 对象"), "acc-home");
    await saveIn("每天最多发几条");
    await cap.findByRole("status");
    // 只发这一节：日上限改了，暂停里没保存的草稿不带上
    expect(s.puts.at(-1)!.body.disturb).toMatchObject({ daily_cap: 3, mutes: [], merge_window_sec: 1800 });
    await saveIn("暂停提醒");
    await m.findByRole("status");
    expect(s.puts.at(-1)!.body.disturb).toMatchObject({ daily_cap: 3, mutes: [{ scope: "project", id: "acc-home" }], merge_window_sec: 1800 });

    await userEvent.type(m.getByLabelText("暂停1 到期"), "2030-01-01T10:00");
    await saveIn("暂停提醒");
    await waitFor(() => expect(s.doc.disturb.mutes[0].until).toBe(new Date("2030-01-01T10:00").toISOString()));

    await userEvent.click(m.getByRole("button", { name: "恢复 1" }));
    await saveIn("暂停提醒");
    await userEvent.click(cap.getByLabelText("不限"));
    await saveIn("每天最多发几条");
    await waitFor(() => expect(s.doc.disturb).toMatchObject({ daily_cap: 0, mutes: [] }));
  });

  it("暂停提醒的来源下拉按名字列出（本人反馈），选中后提交 id；不出现 src_ 原始 id", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await section("暂停提醒");
    await userEvent.click(d.getByRole("button", { name: "＋ 暂停一个" }));
    await userEvent.selectOptions(d.getByLabelText("暂停1 类型"), "source");
    const pick = d.getByLabelText("暂停1 对象");
    await waitFor(() => expect(within(pick).getByRole("option", { name: "Apple Calendar" })).toHaveValue("src_cal_apple"));
    expect(within(pick).getAllByRole("option").map((o) => o.textContent)).toEqual(["", "Apple Calendar", "QQ Mail"]);
    await userEvent.selectOptions(pick, "src_mail_qq");
    await saveIn("暂停提醒");
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb.mutes).toEqual([{ scope: "source", id: "src_mail_qq" }]));
  });

  it("日上限不是非负整数时在本地提示，不发请求", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await section("每天最多发几条");
    await userEvent.clear(d.getByLabelText("每天最多发（条）"));
    await userEvent.type(d.getByLabelText("每天最多发（条）"), "-2");
    await saveIn("每天最多发几条");
    expect(await d.findByRole("alert")).toHaveTextContent("每天最多发的条数要是不小于 0 的整数");
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

  it("安静时段与更多里的开关：各自只发改过的字段，其余原样带回；关掉安静时段就去掉 quiet", async () => {
    const s = policyServer();
    mount(s.impl);
    const q = await section("安静时段");
    await userEvent.click(q.getByLabelText("开启安静时段"));
    await saveIn("安静时段");
    await q.findByRole("status");
    let body = s.puts.at(-1)!.body.disturb;
    expect(body.quiet).toMatchObject({ start: "22:00", end: "08:00" });
    expect(typeof body.quiet.zone).toBe("string");
    expect(body).toMatchObject({ merge_window_sec: 1800, quiet_exempt: true, routes: { decision: "inject", health: "web" } });
    const more = await section("更多");
    await userEvent.click(more.getByLabelText("截止后还没处理，再提醒一次"));
    await userEvent.selectOptions(more.getByLabelText("低优先级提醒合并"), "15");
    await saveIn("更多");
    await more.findByRole("status");
    body = s.puts.at(-1)!.body.disturb;
    expect(body).toMatchObject({ remind_on_deadline: true, merge_window_sec: 900, quiet_exempt: true, routes: { decision: "inject", health: "web" } });
    expect(body).not.toHaveProperty("threshold_judge");
    await userEvent.click(q.getByLabelText("开启安静时段"));
    await saveIn("安静时段");
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb).not.toHaveProperty("quiet"));
  });

  it("唤醒 Agent只在高级里出现，选项里多一个「唤醒 Agent」", async () => {
    mount(policyServer().impl);
    const d = await card();
    expect(within(d.getByLabelText("决策")).queryByRole("option", { name: "唤醒 Agent" })).toBeNull();
    const wake = (await section("更多")).getByLabelText("唤醒 Agent");
    expect(within(wake).getByRole("option", { name: "唤醒 Agent" })).toBeInTheDocument();
  });

  it("推送哪些事：默认要动手的开、纯信息的关；逐类开关与选渠道，保存发 disturb.push，其余字段原样带回", async () => {
    const s = policyServer();
    mount(s.impl);
    const push = await section("推送哪些事");
    expect(push.getByRole("switch", { name: "来源连续失败" })).toBeChecked();
    expect(push.getByRole("switch", { name: "简报或定时任务没出来" })).toBeChecked();
    expect(push.getByRole("switch", { name: "Agent 运行失败" })).toBeChecked();
    expect(push.getByRole("switch", { name: "新建议攒多了" })).not.toBeChecked();
    await userEvent.click(push.getByRole("switch", { name: "新建议攒多了" }));
    await userEvent.clear(push.getByLabelText("攒够几条"));
    await userEvent.type(push.getByLabelText("攒够几条"), "5");
    await userEvent.click(push.getByRole("switch", { name: "Agent 运行失败" }));
    await waitFor(() => expect(options(push.getByLabelText("来源连续失败 送到哪"))).toEqual(["", "channel:slack"]));
    await userEvent.selectOptions(push.getByLabelText("来源连续失败 送到哪"), "channel:slack");
    await saveIn("推送哪些事");
    await push.findByRole("status");
    const d = s.puts.at(-1)!.body.disturb;
    expect(d.push).toEqual({ source_failing: { on: true, route: "channel:slack" }, template_failed: { on: true }, run_failed: { on: false }, new_proposals: { on: true, min: 5 } });
    expect(d.routes).toBeDefined(); // 其余字段原样带回
    expect(screen.getByRole("main")).not.toHaveTextContent(/channel:|source_failing/);
  });
});
