import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { policyServer } from "../settings-policy/test-server";

// 设置 › 提醒与推送（设计 v2 §3.1、§5）：只有全局项与一张只读总览。数据取线上 v28 的形状（test-server 的 V28）。
function mount(fetchImpl: typeof fetch, at = "/settings/notify") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const section = async (name: string) => within(await screen.findByRole("region", { name }));
const saveIn = async (name: string) => userEvent.click((await section(name)).getByRole("button", { name: `保存${name}` }));
const options = (el: HTMLElement) => within(el).getAllByRole("option").map((o) => o.getAttribute("value"));

beforeEach(() => sessionStorage.clear());

describe("设置 › 提醒与推送：只留全局项", () => {
  it("首屏是暂停所有提醒、默认渠道、安静时段（含临近截止照发）、每天上限；暂停名单与合并窗口收在「更多」；没有矩阵、产生开关、模型判断开关与内核词", async () => {
    mount(policyServer({ v28: true }).impl);
    for (const name of ["暂停所有提醒", "默认渠道", "安静时段", "每天最多推几条", "所有提醒一览"]) await section(name);
    const more = screen.getByText("更多").closest("details")!;
    expect(more).not.toHaveAttribute("open");
    expect(within(more).getByRole("region", { name: "暂停名单" })).toBeInTheDocument();
    expect(within(more).getByRole("region", { name: "不急的提醒合成一条" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "每类提醒送到哪" })).toBeNull();
    expect(screen.queryAllByRole("radiogroup")).toEqual([]); // 各类提醒的单选不在这页
    const main = screen.getByRole("main");
    expect(main).not.toHaveTextContent(/产生|让模型判断|推给 Agent|Agent 收不到时|跟随类型|节流|主会话|inject|channel:/);
    expect(screen.getByRole("checkbox", { name: "临近截止的问题照样发" })).toBeChecked();
    expect((await section("安静时段")).getByText("23:00–08:00 不推，结束后合成一条补发；系统告警照发。")).toBeInTheDocument();
  });

  it("暂停所有提醒复用暂停名单：开启写一条 scope all（可带到期），按来源等的暂停原样保留，名单里不列 all", async () => {
    const s = policyServer({ v28: true });
    s.doc.disturb.mutes = [{ scope: "source", id: "src_mail_qq" }];
    mount(s.impl);
    const p = await section("暂停所有提醒");
    expect(p.getByText("关着：按各页的设置提醒。打开后，恢复之前的提醒都只在今天页。")).toBeInTheDocument();
    await userEvent.click(p.getByRole("switch", { name: "暂停所有提醒" }));
    expect(p.getByText("开着：提醒只放今天页；问题照进「等你决定」；来源读不到、运行失败等告警照发。")).toBeInTheDocument();
    await userEvent.type(p.getByLabelText("到这个时间自动恢复（留空表示一直暂停）"), "2030-01-01T10:00");
    await saveIn("暂停所有提醒");
    await waitFor(() => expect(s.puts).toHaveLength(1));
    const body = s.puts[0].body.disturb;
    expect(body.mutes).toEqual([{ scope: "source", id: "src_mail_qq" }, { scope: "all", id: "", until: new Date("2030-01-01T10:00").toISOString() }]);
    expect(body).not.toHaveProperty("delivery");
    expect(body).not.toHaveProperty("delivery_saved");
    const list = await section("暂停名单");
    expect(list.getAllByRole("group")).toHaveLength(1); // 只有按来源的那一条
    await userEvent.click(list.getByRole("button", { name: "恢复 1" }));
    await saveIn("暂停名单");
    await waitFor(() => expect(s.puts).toHaveLength(2));
    expect(s.puts[1].body.disturb.mutes).toEqual([{ scope: "all", id: "", until: new Date("2030-01-01T10:00").toISOString() }]);
    await userEvent.click(p.getByRole("switch", { name: "暂停所有提醒" }));
    await saveIn("暂停所有提醒");
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb.mutes).toEqual([]));
  });

  it("默认渠道：写明按当前设置有哪几类推到这里（v28 是 5 类）；改选只写 default_channel，各行去处原样", async () => {
    const s = policyServer({ v28: true, extKeys: [{ id: "k1", kind: "ext", scope: "channel.slack" }] });
    mount(s.impl);
    const d = await section("默认渠道");
    expect(await d.findByText("现在有 5 类会推到这里：有问题等你决定时、任一来源连续读不到时、Agent 运行失败时、复盘没出来、简报没出来")).toBeInTheDocument();
    await waitFor(() => expect(options(d.getByLabelText("默认渠道"))).toEqual(["", "channel:ch-iphone", "channel:slack"]));
    await userEvent.selectOptions(d.getByLabelText("默认渠道"), "channel:slack");
    await saveIn("默认渠道");
    await waitFor(() => expect(s.puts).toHaveLength(1));
    const body = s.puts[0].body.disturb;
    expect(body.delivery).toEqual({ ...s.doc.disturb.delivery, default_channel: "channel:slack" });
    expect(body.delivery.rows).toEqual(policyServer({ v28: true }).doc.disturb.delivery.rows);
    expect(body.routes).toEqual(s.doc.disturb.routes);
  });

  it("只读总览：行表来自服务端，只列有产生方的；每行现在怎么送、按当前数据、去哪改（链到那一项）；不提醒与不会产生的排在最后", async () => {
    mount(policyServer({ v28: true }).impl);
    const o = await section("所有提醒一览");
    await waitFor(() => expect(o.getAllByRole("row").length).toBeGreaterThan(2));
    const rows = o.getAllByRole("row").slice(1).map((r) => within(r).getAllByRole("cell").map((c) => c.textContent));
    expect(rows).toEqual([
      ["有问题等你决定时", "手机和对话都发（iPhone）", "近 7 天 5 个问题，按现在的设置响了 10 次；现在待你决定 0 个", "我来拍板 ›"],
      ["Agent 替你答了", "在对话里马上告诉我", "近 7 天 0 次", "我来拍板 ›"],
      ["到点提醒", "在对话里马上告诉我", "接下来 24 小时没有要提醒的", "自动规则 ›"],
      ["截止已过", "在对话里马上告诉我", "现在没有带截止的事项，开着也暂时不会提醒", "自动规则 ›"],
      ["等待到期跟进", "在对话里马上告诉我", "现在没有在等的事项", "自动规则 ›"],
      ["14 天没动静", "在对话里马上告诉我", "现在没有；最早 12 月 6 日才可能有", "自动规则 ›"],
      ["任一来源连续读不到时", "推到手机（iPhone）", "近 7 天 1 次", "来源 ›"],
      ["Agent 运行失败时", "推到手机（iPhone）", "近 7 天 0 次", "Agent ›"],
      ["简报的结果", "在对话里马上告诉我", "近 7 天 7 条", "定时安排 ›"],
      ["简报没出来", "推到手机（iPhone）", "近 7 天 0 次", "定时安排 ›"],
      ["系统告警", "不提醒", "近 7 天 5 条", "状态 ›"],
      ["新建议攒多了", "不提醒", "近 7 天新增 275 条建议；打开的话至多约推 27 次", "建议 ›"],
      ["复盘的结果", "这份安排的规则没开，到点不会写", "这份安排的规则没开，到点不会写", "定时安排 ›"],
      ["复盘没出来", "这份安排的规则没开，到点不会写", "这份安排的规则没开，到点不会写", "定时安排 ›"],
      ["其他工具自己的定时任务", "不经 APA，推送在那个工具里配；定时安排页也看不到它们"],
    ]);
    const links = o.getAllByRole("link").map((a) => a.getAttribute("href"));
    expect(links).toContain("/settings#reminder-decision");
    expect(links).toContain("/briefs/schedule#reminder-instance_failed:ins_fe17");
    expect(links).toContain("/health#reminder-health");
    expect(o.queryByText(/规则到点时|规则命中时|非内置类型/)).toBeNull(); // 线上没有产生方的行不列
  });
});

describe("设置 › 提醒与推送：每节各自保存", () => {
  it("ACT-DLV-16/24: 日上限与暂停名单各自保存；今天已推的条数显示在上限下面；没改动时保存不可点", async () => {
    const s = policyServer();
    mount(s.impl);
    const cap = await section("每天最多推几条");
    expect(await cap.findByText("今天已推 7 条，上限 20 条")).toBeInTheDocument();
    expect(cap.getByText("超出的当天只放在今天页。")).toBeInTheDocument();
    expect(cap.getByRole("button", { name: "保存每天最多推几条" })).toBeDisabled();
    await userEvent.clear(cap.getByLabelText("每天最多推（条）"));
    await userEvent.type(cap.getByLabelText("每天最多推（条）"), "3");
    expect(cap.getByText("未保存")).toBeInTheDocument();
    const m = await section("暂停名单");
    await userEvent.click(m.getByRole("button", { name: "＋ 暂停一个" }));
    expect(options(m.getByLabelText("暂停1 类型"))).toEqual(["source", "project", "item"]); // 全部暂停是上面的总开关，不在名单里选
    await userEvent.selectOptions(m.getByLabelText("暂停1 类型"), "project");
    await waitFor(() => expect(options(m.getByLabelText("暂停1 对象"))).toContain("acc-home"));
    await userEvent.selectOptions(m.getByLabelText("暂停1 对象"), "acc-home");
    await saveIn("每天最多推几条");
    await cap.findByRole("status");
    // 只发这一节：日上限改了，暂停里没保存的草稿不带上
    expect(s.puts.at(-1)!.body.disturb).toEqual({ routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 3, mutes: [] });
    await saveIn("暂停名单");
    await m.findByRole("status");
    expect(s.puts.at(-1)!.body.disturb).toEqual({ routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 3, mutes: [{ scope: "project", id: "acc-home" }] });
    await userEvent.type(m.getByLabelText("暂停1 到期"), "2030-01-01T10:00");
    await saveIn("暂停名单");
    await waitFor(() => expect(s.doc.disturb.mutes[0].until).toBe(new Date("2030-01-01T10:00").toISOString()));
    await userEvent.click(m.getByRole("button", { name: "恢复 1" }));
    await saveIn("暂停名单");
    await userEvent.click(cap.getByLabelText("不限"));
    await saveIn("每天最多推几条");
    await waitFor(() => expect(s.doc.disturb).toMatchObject({ daily_cap: 0, mutes: [] }));
  });

  it("暂停名单的来源下拉按名字列出，选中后提交 id；不出现 src_ 原始 id", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await section("暂停名单");
    await userEvent.click(d.getByRole("button", { name: "＋ 暂停一个" }));
    await userEvent.selectOptions(d.getByLabelText("暂停1 类型"), "source");
    const pick = d.getByLabelText("暂停1 对象");
    await waitFor(() => expect(within(pick).getByRole("option", { name: "Apple Calendar" })).toHaveValue("src_cal_apple"));
    expect(within(pick).getAllByRole("option").map((o) => o.textContent)).toEqual(["", "Apple Calendar", "QQ Mail"]);
    await userEvent.selectOptions(pick, "src_mail_qq");
    await saveIn("暂停名单");
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb.mutes).toEqual([{ scope: "source", id: "src_mail_qq" }]));
  });

  it("日上限不是非负整数时在本地提示，不发请求", async () => {
    const s = policyServer();
    mount(s.impl);
    const d = await section("每天最多推几条");
    await userEvent.clear(d.getByLabelText("每天最多推（条）"));
    await userEvent.type(d.getByLabelText("每天最多推（条）"), "-2");
    await saveIn("每天最多推几条");
    expect(await d.findByRole("alert")).toHaveTextContent("每天最多推的条数要是不小于 0 的整数");
    expect(s.puts).toHaveLength(0);
  });

  it("安静时段与合并窗口：各自只发改过的字段，其余原样带回；关掉安静时段就去掉 quiet", async () => {
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
    expect(body).not.toHaveProperty("delivery");
    await userEvent.click(screen.getByText("更多"));
    const m = await section("不急的提醒合成一条");
    await userEvent.selectOptions(m.getByLabelText("不急的提醒合成一条"), "15");
    await saveIn("不急的提醒合成一条");
    await m.findByRole("status");
    body = s.puts.at(-1)!.body.disturb;
    expect(body).toMatchObject({ merge_window_sec: 900, quiet_exempt: true, routes: { decision: "inject", health: "web" } });
    expect(body).not.toHaveProperty("remind_on_deadline");
    expect(body).not.toHaveProperty("threshold_judge");
    expect(body).not.toHaveProperty("followup_defer");
    await userEvent.click(q.getByLabelText("开启安静时段"));
    await saveIn("安静时段");
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb).not.toHaveProperty("quiet"));
  });
});
