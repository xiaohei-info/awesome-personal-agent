import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { setRawShown } from "../../app/raw";
import type { Feature } from "../../app/registry";
import { feature as sources } from "../sources";
import { RecentMessages } from "../sources/Recent";
import { near, type TraceData } from "./Trace";

// 测试边界替身：按 GET /v1/source-events/{id}/trace 的契约（附录 B.2「处理经过的行」）应答，数据照真实部署里的三条典型消息合成。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const OWNER = { type: "human", principal: "owner", via: "web" };
const KERNEL = { type: "system", principal: "kernel:inbound", via: "source" };
const step = (seq: number, type: string, object: string, actor = KERNEL) => ({ seq, type, object, actor, at: "2030-01-01T09:20:07Z", reason: "screened in", payload: { disposition: "proposed" } });

// A：被规则忽略。
const RULE: TraceData = {
  message: { id: "sev_a", source: "src_gh", type: "pr.merged", subject: "改为选填", sender: "alice", sender_verified: false, disposition: "ignored", occurred_at: "2030-01-01T09:00:00Z", arrived_at: "2030-01-01T09:20:00Z" },
  entries: [
    { at: "2030-01-01T09:20:00Z", who: { kind: "source", name: "src_gh" }, kind: "arrived", args: { type: "pr.merged", sender: "alice", verified: false }, steps: [] },
    { at: "2030-01-01T09:20:00Z", who: { kind: "rule", name: "inbound-triage/pr-activity" }, kind: "rule", args: { then: "ignore" }, steps: [step(1, "inbound.disposed", "sev_a")] },
  ],
};
// B：判断模型忽略，之后抽查「确实不用」；另一条的判断记录已过保留期。
const MODEL: TraceData = {
  message: { id: "sev_b", source: "src_mail", type: "mail.received", subject: "领取奖励", sender: "x@example.invalid", sender_verified: false, disposition: "ignored", occurred_at: "2030-01-01T09:00:00Z" },
  entries: [
    { at: "2030-01-01T09:20:00Z", who: { kind: "source", name: "src_mail" }, kind: "arrived", args: { sender: "x@example.invalid", verified: false }, steps: [] },
    { at: "2030-01-01T09:20:00Z", who: { kind: "rule", name: "inbound-triage/mail-received" }, kind: "rule", args: { then: "judge" }, steps: [] },
    { at: "2030-01-01T09:20:00Z", who: { kind: "model", name: "jev-1", role: "judge" }, kind: "judged", args: { levels: [{ backend: "classifier", model: "jev-1", p: 0.11, seg: "below", outcome: "decided" }], low: 0.15, high: 0.6, decided_by: "classifier", answer: "no" }, steps: [step(2, "inbound.disposed", "sev_b")] },
    { at: "2030-01-02T10:00:00Z", who: { kind: "owner" }, kind: "spot", args: { verdict: "correct" }, steps: [step(3, "inbound.spot_reviewed", "sev_b", OWNER)] },
  ],
};
// C：判断模型提成建议，起草失败，同一 PR 的三条后续消息挂上（机械记录，折叠），本人驳回。
const follow = (n: number) => ({ at: `2030-01-01T09:2${n}:00Z`, who: { kind: "source", name: "src_gh" }, kind: "follow", mech: true, args: { state: "attached", subject: `评论 ${n}` }, steps: [step(10 + n, "followmsg.attached", "itm_1")] });
const PROPOSED: TraceData = {
  message: { id: "sev_c", source: "src_gh", type: "pr.opened", subject: "修登录超时", sender: "alice", sender_verified: true, disposition: "proposed", occurred_at: "2030-01-01T09:00:00Z" },
  entries: [
    { at: "2030-01-01T09:20:00Z", who: { kind: "source", name: "src_gh" }, kind: "arrived", args: { sender: "alice", verified: true }, steps: [] },
    { at: "2030-01-01T09:20:00Z", who: { kind: "model", name: "jev-1", role: "judge" }, kind: "judged", args: { levels: [{ backend: "classifier", model: "jev-1", p: 0.68, seg: "above", outcome: "decided" }], low: 0.15, high: 0.6, decided_by: "classifier", answer: "yes" }, steps: [] },
    { at: "2030-01-01T09:20:07Z", who: { kind: "system" }, kind: "proposed", item: { id: "itm_1", title: "修登录超时" }, args: {}, steps: [step(4, "inbound.disposed", "sev_c")] },
    { at: "2030-01-01T09:20:07Z", who: { kind: "model", role: "generate" }, kind: "drafted", args: { outcome: "failed", code: "schema" }, steps: [] },
    follow(1), follow(2), follow(3),
    { at: "2030-01-01T09:35:00Z", who: { kind: "owner" }, kind: "dismissed", item: { id: "itm_1", title: "修登录超时" }, steps: [step(20, "item.phase_changed", "itm_1", OWNER)] },
  ],
};
// 没记的环节：判断记录过期、旧数据不知道是否起草、还在积压。
const GAPS: TraceData = {
  message: { id: "sev_d", source: "src_gh", type: "issue.opened", subject: "旧消息", disposition: "proposed", sender_verified: false, occurred_at: "2030-01-01T09:00:00Z" },
  entries: [
    { at: "2030-01-01T09:20:00Z", who: { kind: "source", name: "src_gh" }, kind: "arrived", args: {}, steps: [] },
    { at: "2030-01-01T09:20:00Z", who: { kind: "system" }, kind: "backlog", args: { reason: "model_unavailable" }, steps: [] },
    { at: "2030-01-01T10:20:00Z", who: { kind: "model", role: "judge" }, kind: "judged", args: { expired: true, answer: "yes", decided_by: "classifier" }, steps: [] },
    { at: "2030-01-01T10:20:00Z", who: { kind: "system" }, kind: "drafted", args: { outcome: "unknown" }, steps: [] },
  ],
};
const TRACES: Record<string, TraceData> = { sev_a: RULE, sev_b: MODEL, sev_c: PROPOSED, sev_d: GAPS };

function backend() {
  return (async (url: string) => {
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_gh", name: "GitHub" }, { id: "src_mail", name: "gmail 邮件" }] });
    const m = /^\/v1\/source-events\/([^/]+)\/trace$/.exec(url);
    if (m && TRACES[m[1]]) return json(200, TRACES[m[1]]);
    if (url.startsWith("/v1/sources/src_gh/events?")) {
      const p = new URL(url, "http://x").searchParams;
      const evs = [PROPOSED, RULE].map((t) => t.message!).filter((e) => !p.get("disposition") || p.get("disposition")!.split(",").includes(e.disposition));
      return json(200, { events: evs, ...(p.get("sort") === "recent" ? {} : { bad: true }) });
    }
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
}

function mount(at: string, features: Feature[] = [sources]) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={features} fetchImpl={backend()} />
    </MemoryRouter>,
  );
}
const lines = async () => within(await screen.findByRole("list", { name: "处理经过" })).getAllByRole("listitem").map((li) => li.textContent);

beforeEach(() => sessionStorage.clear());
afterEach(() => setRawShown(false));

describe("一条消息的处理经过（/sources/events/:id）", () => {
  it("被规则忽略：从哪进来、命中的规则用名字称呼、直接忽略", async () => {
    mount("/sources/events/sev_a");
    expect(await screen.findByRole("heading", { name: "改为选填" })).toBeInTheDocument();
    expect(await lines()).toEqual([expect.stringMatching(/^GitHub收到 alice 的消息 · 未核实/), expect.stringMatching(/^APA 规则「PR 的合并、关闭与更新不用看」直接忽略/)]);
  });

  it("被判断模型忽略：模型名、把握与门槛段用人话写，之后本人抽查「确实不用」", async () => {
    mount("/sources/events/sev_b");
    const l = await lines();
    expect(l[1]).toMatch(/^APA 规则「邮件交给判断模型」交给判断模型/);
    expect(l[2]).toMatch(/^判断模型 jev-1值得看的可能 11%，低于 15%：把握高，直接忽略/);
    expect(l[3]).toMatch(/^你抽查时点了「确实不用」/);
  });

  it("成了建议并被驳回：链到建议、起草失败如实写、同类后续消息折成一行可展开", async () => {
    mount("/sources/events/sev_c");
    const l = await lines();
    expect(l).toHaveLength(6); // 三条后续消息折成一行
    expect(l[1]).toMatch(/高于 60%：把握高，提成建议/);
    expect(screen.getByRole("link", { name: "修登录超时" })).toHaveAttribute("href", "/items/itm_1");
    expect(l[3]).toMatch(/^写作模型起草失败（输出格式不对），用了原标题/);
    expect(l[4]).toMatch(/^GitHub后续消息挂到这里「评论 1」 等 2 条/);
    expect(l[5]).toMatch(/^你驳回了建议/);
    await userEvent.click(screen.getByRole("button", { name: "等 2 条" }));
    expect(await lines()).toHaveLength(8);
    expect(screen.getByRole("button", { name: "收起" })).toHaveAttribute("aria-expanded", "true");
  });

  it("没记的环节如实写：还在积压、判断记录过期、不清楚是否起草", async () => {
    mount("/sources/events/sev_d");
    const l = await lines();
    expect(l[0]).toMatch(/^GitHub收到一条消息/);
    expect(l[1]).toMatch(/还没判断：模型连不上，在积压里/);
    expect(l[2]).toMatch(/判断记录已过保留期，结论是提成建议/);
    expect(l[3]).toMatch(/不清楚是否起草/);
  });

  it("内核事件名、id、载荷只在「显示原始值」里", async () => {
    const { unmount } = mount("/sources/events/sev_c");
    await screen.findByRole("list", { name: "处理经过" });
    const text = () => screen.getByRole("list", { name: "处理经过" }).textContent!;
    expect(text()).not.toMatch(/inbound\.disposed|sev_c|itm_1|screened in|kernel:/);
    unmount();
    setRawShown(true);
    mount("/sources/events/sev_c");
    await screen.findByRole("list", { name: "处理经过" });
    expect(text()).toMatch(/inbound\.disposed/);
    expect(text()).toMatch(/screened in/);
  });
});

describe("消息来源 › 最近的消息", () => {
  const recent: Feature = { id: "recent", title: { zh: "最近", en: "Recent" }, path: "recent", order: 0, section: "sources", element: <RecentMessages source="src_gh" /> };
  it("从新到旧列出；点一行就地展开经过；能单独打开；按结果筛选", async () => {
    mount("/recent", [recent, sources]);
    const list = within(await screen.findByRole("list", { name: "最近的消息" }));
    const row = list.getByRole("button", { name: /修登录超时/ });
    expect(row).toHaveTextContent("已成建议");
    await userEvent.click(row);
    expect(row).toHaveAttribute("aria-expanded", "true");
    const region = within(await screen.findByRole("region", { name: "「修登录超时」的处理经过" }));
    expect(await region.findByText(/驳回了建议/)).toBeInTheDocument();
    expect(region.getByRole("link", { name: "单独打开" })).toHaveAttribute("href", "/sources/events/sev_c");
    await userEvent.click(screen.getByRole("button", { name: "忽略" }));
    expect(await screen.findByRole("button", { name: /改为选填/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /修登录超时/ })).not.toBeInTheDocument();
  });
});

describe("时间就近显示", () => {
  const now = new Date("2030-01-02T12:00:00");
  it("同一分钟不重复；今天只写时分；更早写日期", () => {
    expect(near("2030-01-02T09:20:30", "2030-01-02T09:20:05", now)).toBe("");
    expect(near("2030-01-02T09:21:00", "2030-01-02T09:20:05", now)).toMatch(/^09:21$/);
    expect(near("2029-12-30T09:21:00", undefined, now)).toMatch(/12.*30/);
  });
});
