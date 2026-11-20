import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import type { Item } from "../items/ItemsPage";
import { entries, lanes, ordered, range, type Entry } from "./layout";
import { feature } from "./index";

const OFF = "+08:00";
const TODAY = "2030-01-09"; // 周三
const NOW = Date.parse("2030-01-09T12:00:00+08:00");
const mk = (id: string, over: Partial<Item> = {}): Item => ({ id, phase: "open", version: 1, kind: "task", title: "事项 " + id, ...over });
const days = (m: Map<string, Entry[]>) => Object.fromEntries([...m].map(([d, es]) => [d, es.map((e) => `${e.item.id}:${e.type}`)]));

describe("日历的取材（layout）", () => {
  it("范围：周从周一起七天，月是 1 号所在周的周一起 6×7 格，含首尾", () => {
    expect(range("day", TODAY)).toMatchObject({ from: TODAY, to: TODAY });
    expect(range("week", TODAY)).toMatchObject({ from: "2030-01-07", to: "2030-01-13" });
    const m = range("month", "2030-01-20");
    expect(m).toMatchObject({ from: "2029-12-31", to: "2030-02-10" });
    expect(m.days).toHaveLength(42);
  });

  it("有日期没时段是未定时、不占时间格；没日期也没截止的不上日历；建议、不做了、简报不显示", () => {
    const m = entries([
      mk("untimed", { planned_for: TODAY }),
      mk("timed", { planned_for: TODAY, planned_slot: "09:00 - 10:00" }),
      mk("unscheduled"),
      mk("proposed", { phase: "proposed", planned_for: TODAY }),
      mk("dropped", { phase: "dropped", planned_for: TODAY }),
      mk("brief", { kind: "brief", planned_for: TODAY }),
    ], TODAY, OFF, NOW);
    expect(days(m)).toEqual({ [TODAY]: ["untimed:plan", "timed:plan"] });
    const [u, t] = m.get(TODAY)!;
    expect(u.slot).toBeNull();
    expect(t.slot).toEqual([540, 600]);
  });

  it("截止：同一天只出一条并带截止标记；不同天另出截止标记；只到日的不写时刻；按主机时区落日", () => {
    const m = entries([
      mk("same", { planned_for: TODAY, deadline: "2030-01-09T15:59:59Z" }), // 主机时间 23:59:59：只到日
      mk("split", { planned_for: "2030-01-08", deadline: "2030-01-10T10:00:00Z" }), // 主机 18:00
      mk("late", { planned_for: "2030-01-12", deadline: "2030-01-10T15:59:59Z" }),
      mk("edge", { deadline: "2030-01-10T16:00:00Z" }), // UTC 还是 10 号，主机已是 11 号 00:00
    ], TODAY, OFF, NOW);
    expect(days(m)).toEqual({ [TODAY]: ["same:plan"], "2030-01-08": ["split:plan"], "2030-01-10": ["split:due", "late:due"], "2030-01-12": ["late:plan"], "2030-01-11": ["edge:due"] });
    expect(m.get(TODAY)![0]).toMatchObject({ dueHere: true, due: { date: TODAY, time: null } });
    expect(m.get("2030-01-10")![0].due).toEqual({ date: "2030-01-10", time: "18:00" });
    expect(m.get("2030-01-12")![0].lateVsDue).toBe(true);
  });

  it("过期未办与已过截止：计划早于今天且未完成才算过期；完成的不算已过截止", () => {
    const m = entries([
      mk("over", { planned_for: "2030-01-08" }),
      mk("doneOld", { phase: "done", planned_for: "2030-01-08", deadline: "2030-01-01T00:00:00Z" }),
      mk("missed", { planned_for: TODAY, deadline: "2030-01-09T01:00:00Z" }),
    ], TODAY, OFF, NOW);
    const [over, doneOld] = m.get("2030-01-08")!;
    expect(over.overdue).toBe(true);
    expect([doneOld.overdue, doneOld.pastDue]).toEqual([false, false]);
    expect(m.get(TODAY)![0].pastDue).toBe(true);
  });

  it("同时段冲突：并排分栏、标时间重叠，超过三栏的收进第三栏的 +n；首尾相接的不算重叠", () => {
    const es = (slots: string[]) => [...entries(slots.map((s, i) => mk("e" + i, { planned_for: TODAY, planned_slot: s })), TODAY, OFF, NOW).get(TODAY)!];
    const two = lanes(es(["09:00 - 10:00", "09:30 - 11:00", "11:00 - 12:00"]));
    expect(two.blocks.map((b) => [b.entry.item.id, b.col, b.cols, b.overlap])).toEqual([["e0", 0, 2, true], ["e1", 1, 2, true], ["e2", 0, 1, false]]);
    const many = lanes(es(["09:00 - 10:00", "09:00 - 10:00", "09:00 - 10:00", "09:00 - 10:00", "09:15 - 09:45"]));
    expect(many.blocks.map((b) => b.col)).toEqual([0, 1]);
    expect(many.blocks.every((b) => b.cols === 3 && b.overlap)).toBe(true);
    expect(many.overflow).toHaveLength(1);
    expect(many.overflow[0].entries).toHaveLength(3);
  });

  it("格子里的顺序：截止 → 有时段（按开始）→ 未定时（按优先级，同级按标题），已完成的在各组末尾", () => {
    const m = entries([
      mk("u-low", { planned_for: TODAY, priority: "low", title: "a" }),
      mk("u-high", { planned_for: TODAY, priority: "high", title: "z" }),
      mk("u-done", { planned_for: TODAY, priority: "urgent", phase: "done" }),
      mk("t-late", { planned_for: TODAY, planned_slot: "15:00 - 16:00" }),
      mk("t-early", { planned_for: TODAY, planned_slot: "08:00 - 09:00" }),
      mk("due", { deadline: "2030-01-09T15:59:59Z" }),
    ], TODAY, OFF, NOW);
    expect(ordered(m.get(TODAY)).map((e) => e.item.id)).toEqual(["due", "t-early", "t-late", "u-high", "u-low", "u-done"]);
  });
});

// ---- 页面：取范围、视图记忆与地址优先、点击落点、"还有 n 件"、窄屏周列表 ----

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function server(items: Item[]) {
  const calls: string[] = [];
  const impl = (async (url: string) => {
    calls.push(url);
    if (url === "/v1/projects") return json({ projects: [{ id: "p1", name: "家务" }] });
    if (url === "/v1/today") return json({ planned: [mk("old", { planned_for: "2030-01-02" })], due: [], pending: [], stale: [] });
    if (url.startsWith("/v1/items?")) {
      const q = new URL(url, "http://x").searchParams;
      const from = q.get("from")!, to = q.get("to")!;
      const hit = q.get("kind") ? [] : items.filter((i) => (i.planned_for ?? "") >= from && (i.planned_for ?? "") <= to);
      return json({ items: hit, last_rejections: {}, today: TODAY, utc_offset: OFF });
    }
    return json({});
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(f: typeof fetch, path: string) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={[path]}><App features={[feature]} fetchImpl={f} /></MemoryRouter>);
}
const ranges = (calls: string[]) => calls.filter((c) => c.startsWith("/v1/items?") && !c.includes("kind=")).map((c) => {
  const q = new URL(c, "http://x").searchParams;
  return `${q.get("from")}..${q.get("to")}`;
});

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

describe("日历页", () => {
  it("第一次打开是周视图并按周读范围；切到月后记住，地址带了参数就以地址为准", async () => {
    const s = server([mk("it_x9", { title: "开会", planned_for: TODAY, planned_slot: "09:00 - 10:00", project: "p1" })]);
    const v = mount(s.impl, "/calendar");
    expect(await screen.findByRole("button", { name: "周", pressed: true })).toBeInTheDocument();
    await waitFor(() => expect(ranges(s.calls)).toContain("2030-01-07..2030-01-13"));
    const link = await screen.findByRole("link", { name: /开会/ });
    expect(link).toHaveAttribute("href", "/items/it_x9"); // 点开直接进事项详情
    expect(link.getAttribute("aria-label")).toContain("家务"); // 悬停卡与读屏写项目名，不写 id
    expect(link.textContent).not.toContain("it_x9");
    expect(link.getAttribute("aria-label")).not.toContain("it_x9");
    expect(await screen.findByText(/1 件过期未办/)).toBeInTheDocument(); // 与今天页第一组同一口径
    await userEvent.click(screen.getByRole("button", { name: "月" }));
    await waitFor(() => expect(ranges(s.calls)).toContain("2029-12-31..2030-02-10"));
    v.unmount();

    const again = server([]);
    const w = mount(again.impl, "/calendar");
    expect(await screen.findByRole("button", { name: "月", pressed: true })).toBeInTheDocument();
    w.unmount();
    mount(server([]).impl, "/calendar?view=day&date=2030-01-20");
    expect(await screen.findByRole("button", { name: "日", pressed: true })).toBeInTheDocument();
    expect(await screen.findByText("这段时间还没有排期的事")).toBeInTheDocument();
  });

  it("周视图的未定时栏每列最多 3 条，多出的写还有 n 件，点开进当天的日视图", async () => {
    const s = server(["a", "b", "c", "d"].map((id) => mk(id, { planned_for: TODAY })));
    mount(s.impl, "/calendar?view=week&date=" + TODAY);
    await userEvent.click(await screen.findByRole("button", { name: "还有 1 件" }));
    expect(await screen.findByRole("button", { name: "日", pressed: true })).toBeInTheDocument();
    await waitFor(() => expect(ranges(s.calls)).toContain(`${TODAY}..${TODAY}`));
    expect(screen.getAllByRole("link", { name: /^事项 [abcd]/ })).toHaveLength(4); // 日视图的未定时栏全部列出
  });

  it("点空白的半小时格去新建页，预填那天与往后一小时的时段（最晚 24:00）", async () => {
    mount(server([]).impl, "/calendar?view=day&date=" + TODAY);
    expect(await screen.findByRole("link", { name: `在 ${TODAY} 09:30 新建` })).toHaveAttribute("href", `/new?planned_for=${TODAY}&planned_slot=09%3A30+-+10%3A30`);
    expect(screen.getByRole("link", { name: `在 ${TODAY} 23:30 新建` })).toHaveAttribute("href", `/new?planned_for=${TODAY}&planned_slot=23%3A30+-+24%3A00`);
  });

  it("窄屏：第一次打开是日视图；周视图改成七天列表，没有安排的那天写没有安排", async () => {
    const mm = window.matchMedia;
    window.matchMedia = ((q: string) => ({ matches: true, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
    try {
      const v = mount(server([]).impl, "/calendar");
      expect(await screen.findByRole("button", { name: "日", pressed: true })).toBeInTheDocument();
      v.unmount();
      mount(server([mk("a", { planned_for: TODAY })]).impl, "/calendar?view=week&date=" + TODAY);
      const week = await screen.findByRole("region", { name: "2030-01-07 – 2030-01-13" });
      await within(week).findByRole("link", { name: /事项 a/ });
      expect(within(week).getAllByText("没有安排")).toHaveLength(6);
    } finally {
      window.matchMedia = mm;
    }
  });
});
