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
const CARD = "每类提醒送到哪";
const card = async () => within(await screen.findByRole("region", { name: CARD }));
const save = async () => userEvent.click((await card()).getByRole("button", { name: `保存${CARD}` }));
// 每一节单独保存（界面设计 §6.1）：保存发的是整段 disturb，已保存的原样带回，只换这一节改过的字段。
const section = async (name: string) => within(await screen.findByRole("region", { name }));
const saveIn = async (name: string) => userEvent.click((await section(name)).getByRole("button", { name: `保存${name}` }));
const options = (el: HTMLElement) => within(el).getAllByRole("option").map((o) => o.getAttribute("value"));
const row = async (name: string) => within((await card()).getByRole("group", { name }));

/** 在替身外再应答规则列表与切换模式（PATCH /rules/{key}/mode）。 */
function withRules(impl: typeof fetch, rules: { key: string; mode: string }[]) {
  const patches: { url: string; body: any }[] = [];
  const f = (async (url: string, init: RequestInit = {}) => {
    if (url === "/v1/rules") return new Response(JSON.stringify({ rules }), { status: 200, headers: { "Content-Type": "application/json" } });
    if (url.startsWith("/v1/rules/") && init.method === "PATCH") {
      const body = JSON.parse(init.body as string);
      patches.push({ url, body });
      return new Response(JSON.stringify({ key: decodeURIComponent(url.split("/")[3]), mode: body.mode }), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    return impl(url, init);
  }) as unknown as typeof fetch;
  return { impl: f, patches };
}

beforeEach(() => sessionStorage.clear());

describe("设置 › 提醒与推送 › 每类提醒送到哪", () => {
  it("没保存过时按旧键换算显示：下次对话时、推到对话里马上、两边都不推；页面不出现原始取值", async () => {
    mount(policyServer().impl);
    const d = await row("问题卡");
    expect(d.getByLabelText("问题卡：推给 Agent")).toHaveValue("on");
    expect(d.getByLabelText("问题卡：什么时候带上")).toHaveValue("next_chat");
    expect(d.getByLabelText("问题卡：推给你")).toHaveValue("off");
    expect(d.getByText("一直没对话，也会先放在今天页")).toBeInTheDocument();
    const overdue = await row("截止已过");
    expect(overdue.getByLabelText("截止已过：推给 Agent")).toHaveValue("off");
    expect(overdue.getByLabelText("截止已过：推给你")).toHaveValue("off");
    expect((await row("到点提醒")).getByLabelText("到点提醒：什么时候带上")).toHaveValue("main");
    const failed = await row("简报没出来");
    expect(failed.getByLabelText("简报没出来：什么时候带上")).toHaveValue("main");
    expect(failed.getByLabelText("简报没出来：Agent 收不到时")).toHaveValue("next_chat");
    expect((await row("例行任务的结果")).getByText("两边都不推：只记在 APA")).toBeInTheDocument();
    expect(screen.getByRole("main")).not.toHaveTextContent(/inject|next_chat|channel:|主会话/);
  });

  it("内置渠道按名字列出；指向已停用或已删除连接的照样显示并标明；默认渠道取旧路由里用得最多的，主视图不出现连接 id", async () => {
    const s = policyServer({
      connections: [{ id: "ch-1a2b", name: "我的 iPhone", enabled: true }, { id: "ch-off0", name: "旧手机", enabled: false }],
      routes: { decision: "channel:ch-1a2b", brief: "channel:ch-off0", reminder: "channel:ch-gone", health: "web", "decision:urgent": "channel:ch-1a2b" },
    });
    mount(s.impl);
    const c = await card();
    expect(c.getByLabelText("默认渠道")).toHaveValue("channel:ch-1a2b");
    const decision = await row("问题卡");
    await waitFor(() => expect(options(decision.getByLabelText("问题卡：推给你"))).toEqual(["off", "default", "channel:ch-1a2b", "channel:slack"]));
    expect(decision.getByLabelText("问题卡：推给你")).toHaveValue("default");
    expect(within(decision.getByLabelText("问题卡：推给你")).getByRole("option", { name: "默认渠道（我的 iPhone）" })).toBeInTheDocument();
    const overdue = await row("截止已过");
    expect(overdue.getByLabelText("截止已过：推给你")).toHaveValue("channel:ch-gone");
    expect(within(overdue.getByLabelText("截止已过：推给你")).getByRole("option", { name: "发到已删除的渠道（先放在今天页）" })).toBeInTheDocument();
    expect((await row("其他简报")).getByLabelText("其他简报：推给你")).toHaveValue("channel:ch-off0");
    expect(screen.getByRole("main")).not.toHaveTextContent(/ch-|channel:/);
  });

  it("保存只写 disturb.delivery：冻结的旧键原样带回，按优先级的覆盖不丢；之后别的节保存不再带矩阵", async () => {
    const routes = { decision: "inject", reminder: "web", health: "web", "reminder:high": "channel:slack" };
    const s = policyServer({ routes });
    s.doc.disturb.push = { source_failing: { on: true, route: "channel:slack" } };
    s.doc.disturb.proactive = { on: false, work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6, task_push: { brief: false } };
    mount(s.impl);
    const overdue = await row("截止已过");
    await waitFor(() => expect(options(overdue.getByLabelText("截止已过：推给你"))).toContain("channel:slack"));
    await userEvent.selectOptions(overdue.getByLabelText("截止已过：推给你"), "default");
    expect(overdue.getByText("另有 1 个优先级单独设（保留）")).toBeInTheDocument();
    await save();
    await (await card()).findByText("已保存"); // 矩阵里嵌着到点提醒的预览（也是 status），按保存结果的文字找
    const body = s.puts.at(-1)!.body.disturb;
    expect(body).not.toHaveProperty("delivery_saved");
    expect(body.routes).toEqual(routes);
    expect(body.push).toEqual({ source_failing: { on: true, route: "channel:slack" } });
    expect(body.proactive.task_push).toEqual({ brief: false });
    expect(body.delivery.default_channel).toBe("channel:slack");
    expect(body.delivery.rows.overdue).toEqual({ agent: "off", user: "default", by_priority: { high: { agent: "off", user: "default" } } });
    expect(body.delivery.rows.followup).toEqual({ agent: "off", user: "off", by_priority: { high: { agent: "off", user: "default" } } });
    expect(body.delivery.rows["task_result:brief"]).toEqual({ agent: "off", user: "off" });
    expect(s.doc.disturb.delivery).toEqual(body.delivery);

    const cap = await section("每天最多发几条");
    await userEvent.clear(cap.getByLabelText("每天最多发（条）"));
    await userEvent.type(cap.getByLabelText("每天最多发（条）"), "5");
    await saveIn("每天最多发几条");
    await cap.findByRole("status");
    expect(s.puts.at(-1)!.body.disturb).not.toHaveProperty("delivery");
    expect(s.doc.disturb.delivery.rows.overdue.user).toBe("default"); // 服务端沿用已存的矩阵
  });

  it("两层都推时提示两边都会收到；推到对话里马上时可选收不到时改送哪；改回不推就去掉退路", async () => {
    const s = policyServer();
    mount(s.impl);
    const r = await row("来源连续失败");
    await waitFor(() => expect(options(r.getByLabelText("来源连续失败：推给你"))).toContain("channel:slack"));
    await userEvent.selectOptions(r.getByLabelText("来源连续失败：推给你"), "channel:slack");
    expect(r.getByRole("note")).toHaveTextContent("两边都会收到");
    await userEvent.selectOptions(r.getByLabelText("来源连续失败：什么时候带上"), "main");
    expect(options(r.getByLabelText("来源连续失败：Agent 收不到时"))).toEqual(["", "next_chat", "channel:slack"]);
    await userEvent.selectOptions(r.getByLabelText("来源连续失败：Agent 收不到时"), "next_chat");
    await save();
    await (await card()).findByText("已保存"); // 矩阵里嵌着到点提醒的预览（也是 status），按保存结果的文字找
    expect(s.puts.at(-1)!.body.disturb.delivery.rows.source_failing).toEqual({ agent: "main", user: "channel:slack", else: "next_chat" });
    await userEvent.selectOptions(r.getByLabelText("来源连续失败：推给 Agent"), "off");
    expect(r.queryByLabelText("来源连续失败：Agent 收不到时")).toBeNull();
    await save();
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb.delivery.rows.source_failing).toEqual({ agent: "off", user: "channel:slack" }));
  });

  it("产不产生的开关在对应行：推送类逐行开关（渠道原样带回）、截止后再提醒、两个让模型判断的开关", async () => {
    const s = policyServer();
    s.doc.disturb.push = { source_failing: { on: true, route: "channel:slack" } };
    mount(s.impl);
    const c = await card();
    expect(c.getByRole("switch", { name: "推送「来源连续失败」" })).toBeChecked();
    expect(c.getByRole("switch", { name: "推送「简报或定时任务没出来」" })).toBeChecked();
    expect(c.getByRole("switch", { name: "推送「Agent 运行失败」" })).toBeChecked();
    expect(c.getByRole("switch", { name: "推送「新建议攒多了」" })).not.toBeChecked();
    await userEvent.click(c.getByRole("switch", { name: "推送「新建议攒多了」" }));
    await userEvent.clear(c.getByLabelText("攒够几条"));
    await userEvent.type(c.getByLabelText("攒够几条"), "5");
    await userEvent.click(c.getByRole("switch", { name: "推送「Agent 运行失败」" }));
    await userEvent.click((await row("截止已过")).getByRole("switch", { name: "截止后还没处理，再提醒一次" }));
    await userEvent.click((await row("问题卡")).getByLabelText("让模型判断决策能不能晚点送"));
    await save();
    await c.findByText("已保存");
    const d = s.puts.at(-1)!.body.disturb;
    expect(d.push).toEqual({ source_failing: { on: true, route: "channel:slack" }, template_failed: { on: true }, run_failed: { on: false }, new_proposals: { on: true, min: 5 } });
    expect(d).toMatchObject({ remind_on_deadline: true, threshold_judge: true });
    expect(d).not.toHaveProperty("followup_defer");
    expect(screen.getByRole("main")).not.toHaveTextContent(/channel:|source_failing/);
  });

  it("跟进、停滞两行的产生开关就是规则模式：即改即存（PATCH /rules/{key}/mode）；规则设的提醒链到自动动手", async () => {
    const r = withRules(policyServer().impl, [{ key: "follow-up/clock_rules/follow-up-interval", mode: "auto" }, { key: "follow-up/clock_rules/stale-check", mode: "auto" }]);
    mount(r.impl);
    const stale = await row("14 天没动静");
    const modes = await stale.findByRole("radiogroup", { name: "14 天没动静：开关" });
    expect(within(modes).getByLabelText("开")).toBeChecked();
    await userEvent.click(within(modes).getByLabelText("试运行"));
    await waitFor(() => expect(r.patches).toEqual([{ url: "/v1/rules/follow-up%2Fclock_rules%2Fstale-check/mode", body: { mode: "observe" } }]));
    expect(await stale.findByRole("status")).toHaveTextContent("已保存");
    expect((await row("等待到期跟进")).getByRole("radiogroup", { name: "等待到期跟进：开关" })).toBeInTheDocument();
    expect((await row("规则设的提醒")).getByRole("link", { name: "开关在「自动动手」" })).toHaveAttribute("href", "/settings/rules");
  });

  it("按实例覆盖：旧的 instance_push 换算成实例行，可改回跟随类型；没覆盖的实例显示跟随类型，可单独设", async () => {
    const s = policyServer({ instances: [{ id: "ins_b", pack: "brief", params: { schedule: "0 8 * * *" } }, { id: "ins_r", pack: "review", params: { title: "周复盘" } }, { id: "ins_m", pack: "agents-md", params: {} }] });
    s.doc.disturb.proactive = { on: false, work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6, instance_push: { ins_b: false } };
    mount(s.impl);
    const own = await row("简报 · 0 8 * * *");
    expect(own.getByLabelText("简报 · 0 8 * * *：推给 Agent")).toHaveValue("off");
    const follow = await (await card()).findByRole("group", { name: "复盘 · 周复盘" });
    expect(within(follow).getByText("跟随类型")).toBeInTheDocument();
    expect(screen.queryByText(/agents-md/)).toBeNull();
    await userEvent.click(within(follow).getByRole("button", { name: "单独设" }));
    await userEvent.click(own.getByRole("button", { name: "改回跟随类型" }));
    await save();
    await (await card()).findByText("已保存"); // 矩阵里嵌着到点提醒的预览（也是 status），按保存结果的文字找
    const rows = s.puts.at(-1)!.body.disturb.delivery.rows;
    expect(rows["instance:ins_r"]).toEqual({ agent: "main", user: "off" });
    expect(rows).not.toHaveProperty("instance:ins_b");
    expect(rows["instance_failed:ins_b"]).toEqual({ agent: "next_chat", user: "off" }); // instance_push 为假：没出来跟「需要你决定」（routes.decision = inject）
  });

  it("决策行只读写明代答在我来拍板改；更多里没有「唤醒 Agent」，也没有改旧路由的地方", async () => {
    mount(policyServer().impl);
    expect((await row("问题卡")).getByRole("link", { name: "哪些问题 Agent 可代答：在「我来拍板」改" })).toHaveAttribute("href", "/settings");
    const more = await section("更多");
    expect(more.queryByLabelText("唤醒 Agent")).toBeNull();
    expect(screen.queryByText("唤醒 Agent")).toBeNull();
    expect(screen.queryByRole("region", { name: "各类提醒怎么送" })).toBeNull();
    expect(screen.queryByRole("region", { name: "推送哪些事" })).toBeNull();
    expect(screen.queryByRole("region", { name: "定时任务的结果" })).toBeNull();
  });
});

describe("设置 › 提醒与推送 › 全局节流", () => {
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

  it("安静时段与更多（合并窗口）：各自只发改过的字段，其余原样带回；关掉安静时段就去掉 quiet", async () => {
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
    const more = await section("更多");
    await userEvent.selectOptions(more.getByLabelText("低优先级提醒合并"), "15");
    await saveIn("更多");
    await more.findByRole("status");
    body = s.puts.at(-1)!.body.disturb;
    expect(body).toMatchObject({ merge_window_sec: 900, quiet_exempt: true, routes: { decision: "inject", health: "web" } });
    expect(body).not.toHaveProperty("remind_on_deadline");
    expect(body).not.toHaveProperty("threshold_judge");
    await userEvent.click(q.getByLabelText("开启安静时段"));
    await saveIn("安静时段");
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb).not.toHaveProperty("quiet"));
  });

});

describe("设置 › 提醒与推送 › 到点提醒（矩阵「到点提醒」行的展开区）", () => {
  it("默认关；预览写明开启后下一批几条、在什么时候、24 小时共几条与几条消息、送到主会话；一键开启只发 proactive 且不带 since", async () => {
    const at = new Date(2026, 10, 24, 9, 0).toISOString();
    const s = policyServer({ preview: { total: 5, batches: 3, next_at: at, next_count: 2, route: "wake", entries: [] } });
    mount(s.impl);
    const c = await section("到点提醒");
    expect(c.getByRole("switch", { name: "开启到点提醒" })).not.toBeChecked();
    await waitFor(() => expect(c.getByRole("status", { name: "开启后的预览" })).toHaveTextContent(
      "按当前数据，开启后下一批提醒 2 条，在 11月24日 09:00接下来 24 小时共 5 条，合成 3 条消息送到主 Agent 的对话里开启之前已经过去的提醒点不补推"));
    expect(s.previews.at(-1)).toEqual({ on: false, work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6 });
    await userEvent.click(c.getByRole("button", { name: "按这些设置开启" }));
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb.proactive).toEqual({ on: true, work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6 }));
    expect(s.puts.at(-1)!.body.disturb.routes).toEqual({ decision: "inject", health: "web" }); // 其余段原样带回
  });

  it("主会话不能唤醒时写明原因；改工作开始时间与工作日后预览按草稿重算，保存发整段", async () => {
    const s = policyServer({ preview: { total: 1, batches: 1, next_at: new Date(2026, 10, 24, 9, 0).toISOString(), next_count: 1, route: "web", blocked: "main_cannot_wake", entries: [] } });
    mount(s.impl);
    const c = await section("到点提醒");
    await waitFor(() => expect(c.getByRole("status", { name: "开启后的预览" })).toHaveTextContent("主 Agent 不能在对话里接收，先放在今天页的提醒里"));
    await userEvent.clear(c.getByLabelText("工作开始时间"));
    await userEvent.type(c.getByLabelText("工作开始时间"), "08:30");
    await userEvent.click(within(c.getByRole("group", { name: "工作日" })).getByLabelText("六"));
    await waitFor(() => expect(s.previews.at(-1)).toMatchObject({ work_start: "08:30", workdays: [1, 2, 3, 4, 5, 6] }));
    await saveIn("到点提醒");
    await waitFor(() => expect(s.puts.at(-1)!.body.disturb.proactive).toMatchObject({ on: false, work_start: "08:30", workdays: [1, 2, 3, 4, 5, 6] }));
  });
});
