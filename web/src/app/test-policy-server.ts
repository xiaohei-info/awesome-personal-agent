// Test double for the settings pages that edit the policy document (not shipped: only imported by tests).
// It answers like api/settings/policy: three routes read and replace whole sections, keys are masked,
// GET /agents lists the Agent cards with their permission card,
// and a non-Web session gets 403 web_only (error bodies follow appendix B.3).
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix it; nothing was saved" });

export const SECTIONS: Record<string, string[]> = {
  "/v1/settings/policy": ["disturb", "background_run_timeout_sec", "channels", "default_harness", "main_session_launch", "rule_agent"],
  "/v1/settings/allowlists": ["forward_allowlist", "channel_answerers"],
  "/v1/settings/access-limits": ["agent_cards"],
};

export function initialDoc(): Record<string, any> {
  return {
    disturb: { routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 20, mutes: [] },
    background_run_timeout_sec: 14400,
    channels: {},
    default_harness: "",
    main_session_launch: { by_harness: {}, by_rule: {} },
    forward_allowlist: { telegram: ["alice"] },
    channel_answerers: {},
    agent_cards: {},
  };
}

/**
 * 与 kernel/policy.DeriveDelivery 同一套换算（测试替身用）：disturb 没有 delivery 时，GET 返回按旧键换算出的矩阵。
 * inject → (next_chat, off)；web → (off, off)；channel:X → (off, X 是默认渠道记 default，否则记 X)。
 */
export function deriveDelivery(d: Record<string, any>, packs: Record<string, string> = {}) {
  const routes: Record<string, string> = d.routes ?? {};
  const push: Record<string, { on?: boolean; route?: string }> = d.push ?? {};
  const norm = (r?: string) => (!r ? "web" : ["inject", "wake", "web"].includes(r) || r.startsWith("channel:") ? r : "channel:" + r);
  const n: Record<string, number> = {};
  for (const r of [...Object.values(routes), ...Object.values(push).map((p) => p.route ?? "")]) if (r && norm(r).startsWith("channel:")) n[norm(r)] = (n[norm(r)] ?? 0) + 1;
  const def = Object.keys(n).sort().reduce((b, k) => (!b || n[k] > n[b] ? k : b), "");
  const user = (r?: string) => { const x = norm(r); return x === "web" || x === "inject" ? "off" : x === def ? "default" : x; };
  type T = { agent: string; user: string; else?: string; by_priority?: Record<string, T> };
  const conv = (r?: string): T => (norm(r) === "inject" ? { agent: "next_chat", user: "off" } : { agent: "off", user: user(r) });
  const orElse = (r?: string) => { const c = conv(r); return c.agent === "next_chat" ? "next_chat" : c.user === "off" ? "" : c.user; };
  const main = (r?: string): T => { const e = orElse(r); return { agent: "main", user: "off", ...(e ? { else: e } : {}) }; };
  const from = (cls: string, wrap: (r?: string) => T): T => {
    const t = wrap(routes[cls]);
    for (const [k, r] of Object.entries(routes)) if (k.startsWith(cls + ":")) (t.by_priority ??= {})[k.slice(cls.length + 1)] = wrap(r);
    return t;
  };
  const pr = d.proactive ?? {};
  const pushTask = (t: string, id = "") => (id && id in (pr.instance_push ?? {}) ? pr.instance_push[id] : t in (pr.task_push ?? {}) ? pr.task_push[t] : { brief: true, review: true, routine: false }[t] ?? false);
  const tmpl = push.template_failed?.route;
  const briefPack = (p: string) => p === "brief" || p === "review";
  const result = (p: string, on: boolean): T => (on && briefPack(p) ? from("brief", main) : on ? { agent: "main", user: "off" } : briefPack(p) ? from("brief", conv) : { agent: "off", user: "off" });
  const failed = (on: boolean): T => (tmpl ? conv(tmpl) : on ? main(routes.decision) : conv(routes.decision));
  const pushRoute = (c: string) => (push[c]?.route ? conv(push[c].route) : conv(routes.decision));
  const rows: Record<string, T> = {
    decision: from("decision", conv), scheduled: { agent: "main", user: "off" },
    "task_result:other": from("brief", conv), "task_failed:other": failed(false),
    source_failing: pushRoute("source_failing"), run_failed: pushRoute("run_failed"), new_proposals: pushRoute("new_proposals"), health: conv(routes.health),
  };
  for (const r of ["agent_answered", "overdue", "followup", "stale", "rule_reminder", "source_alert"]) rows[r] = from("reminder", conv);
  for (const t of ["brief", "review", "routine"]) { rows["task_result:" + t] = result(t, pushTask(t)); rows["task_failed:" + t] = failed(pushTask(t)); }
  for (const [id, on] of Object.entries((pr.instance_push ?? {}) as Record<string, boolean>)) if (packs[id]) { rows["instance:" + id] = result(packs[id], on); rows["instance_failed:" + id] = failed(on); }
  return { default_channel: def, rows };
}

// 线上 2026-11-23 23:05 的策略 v28（disturb 段，只读账本取得；渠道连接 id 换成占位）、两个定时安排、规则模式，
// 以及 GET /settings/notify/overview 按同一时刻数据的形状（设计 v2 §1.3 的计数）。各页"用当前数据渲染出预览"的测试都用它。
export const V28 = {
  disturb: {
    routes: { brief: "inject", decision: "channel:ch-iphone", health: "web", reminder: "channel:ch-iphone", wake: "web" },
    quiet: { start: "23:00", end: "08:00", zone: "Asia/Shanghai" }, quiet_exempt: true, merge_window_sec: 1800, remind_on_deadline: true, daily_cap: 20, mutes: [],
    proactive: { on: true, since: "2026-11-23T11:49:29Z", work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6 },
    delivery: {
      default_channel: "channel:ch-iphone",
      rows: {
        agent_answered: { agent: "main", user: "off" }, decision: { agent: "main", user: "default" }, followup: { agent: "main", user: "off" },
        health: { agent: "off", user: "off" }, new_proposals: { agent: "off", user: "default" }, overdue: { agent: "main", user: "off" },
        rule_reminder: { agent: "main", user: "off" }, run_failed: { agent: "off", user: "default" }, scheduled: { agent: "main", user: "off" },
        source_alert: { agent: "off", user: "default" }, source_failing: { agent: "off", user: "default" }, stale: { agent: "main", user: "off" },
        "task_failed:brief": { agent: "off", user: "default" }, "task_failed:other": { agent: "off", user: "default" }, "task_failed:review": { agent: "off", user: "default" },
        "task_failed:routine": { agent: "off", user: "default" }, "task_result:brief": { agent: "main", user: "off", else: "next_chat" },
        "task_result:other": { agent: "next_chat", user: "off" }, "task_result:review": { agent: "main", user: "off", else: "next_chat" }, "task_result:routine": { agent: "main", user: "off" },
      },
    },
  },
  instances: [
    { id: "ins_b6b2", pack: "review", params: { schedule: "30 17 * * 5" }, enabled: true },
    { id: "ins_fe17", pack: "brief", params: { schedule: "CRON_TZ=Asia/Shanghai 0 8 * * *" }, enabled: true },
  ],
  rules: [
    { key: "brief/clock_rules/scheduled-brief", pack: "brief", group: "clock_rules", id: "scheduled-brief", mode: "auto", class: "action" },
    { key: "follow-up/clock_rules/follow-up-interval", pack: "follow-up", group: "clock_rules", id: "follow-up-interval", mode: "auto", class: "reminder" },
    { key: "follow-up/clock_rules/stale-check", pack: "follow-up", group: "clock_rules", id: "stale-check", mode: "auto", class: "reminder" },
    { key: "review/clock_rules/scheduled-review", pack: "review", group: "clock_rules", id: "scheduled-review", mode: "off", class: "action" },
    { key: "routine/clock_rules/scheduled-routine", pack: "routine", group: "clock_rules", id: "scheduled-routine", mode: "observe", class: "action" },
  ],
  connections: [{ id: "ch-iphone", name: "iPhone", enabled: true }],
};

/** 内核 policy.FixedRows 的顺序（行表本身由 homes.check.test.ts 对照内核源码）。 */
const FIXED = ["decision", "agent_answered", "scheduled", "overdue", "followup", "stale", "rule_reminder",
  ...["brief", "review", "routine", "other"].map((t) => "task_result:" + t), ...["brief", "review", "routine", "other"].map((t) => "task_failed:" + t),
  "source_failing", "run_failed", "health", "new_proposals", "source_alert"];

export function v28Overview() {
  const counts: Record<string, [number, number]> = { decision: [5, 10], source_failing: [1, 1], health: [5, 0], "instance:ins_fe17": [7, 7] };
  const unproduced = ["rule_reminder", "source_alert", "task_result:brief", "task_result:review", "task_result:routine", "task_result:other", "task_failed:brief", "task_failed:review", "task_failed:routine", "task_failed:other"];
  const row = (key: string, extra: Record<string, unknown> = {}) => ({ key, produced: !unproduced.includes(key), events: counts[key]?.[0] ?? 0, pushed: counts[key]?.[1] ?? 0, ...extra });
  return {
    since: "2026-11-16T15:05:00Z",
    rows: [
      ...FIXED.map((k) => row(k)),
      row("instance:ins_b6b2", { blocked: "rule_off" }), row("instance_failed:ins_b6b2", { blocked: "rule_off" }),
      row("instance:ins_fe17"), row("instance_failed:ins_fe17"),
    ],
    now: { decisions_pending: 0, deadlines: 0, overdue: 0, waiting: 0, stale_now: 0, stale_from: "2026-12-06T02:00:00Z", proposals_new: 275, rule_observed: { "routine/clock_rules/scheduled-routine": 0 } },
    source_alert: { streak_failures: 3, low_frequency_over_sec: 3600, sources: [
      { id: "src_daxiang", name: "Daxiang", schedule: "60m", alert_after: 3 }, { id: "src_mail", name: "Gmail", schedule: "10m", alert_after: 3 },
    ] },
  };
}

export function policyServer(opts: { preview?: Record<string, unknown>; instances?: Record<string, unknown>[]; webOnlyDenied?: boolean; agents?: Record<string, unknown>[]; connections?: { id: string; name: string; enabled: boolean }[]; routes?: Record<string, string>; extKeys?: Record<string, unknown>[]; v28?: boolean; rules?: { key: string; mode: string }[] } = {}) {
  let version = 1;
  const doc = initialDoc();
  if (opts.v28) {
    version = 28;
    doc.disturb = structuredClone(V28.disturb);
    opts = { instances: V28.instances, connections: V28.connections, rules: V28.rules, extKeys: [], ...opts };
  }
  if (opts.routes) doc.disturb.routes = opts.routes;
  const rules = structuredClone(opts.rules ?? []);
  const patches: { url: string; body: any }[] = [];
  const puts: { path: string; body: any; csrf?: string }[] = [];
  const previews: any[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [{ id: "acc-home", name: "Home" }] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_cal_apple", name: "Apple Calendar" }, { id: "src_mail_qq", name: "QQ Mail" }] });
    if (url === "/v1/settings/ext-keys") return json(200, { credentials: opts.extKeys ?? [{ id: "k1", kind: "ext", scope: "channel.slack" }, { id: "k2", kind: "ext", scope: "channel.old", revoked_at: "x" }] });
    if (url === "/v1/connections?direction=channel") return json(200, { connections: opts.connections ?? [] });
    if (url === "/v1/settings/commands") return json(200, { declarations: [{ harness: "hermes", rev: 1, state: "confirmed", declaration: { harness: "hermes", headless: { command: ["hermes"], access: { read: [], edit: [] } } } }] });
    if (url === "/v1/settings/proactive/preview" && method === "POST") {
      previews.push(JSON.parse(init.body as string));
      return json(200, opts.preview ?? { total: 0, batches: 0, next_count: 0, route: "web", blocked: "main_not_set", entries: [] });
    }
    if (url === "/v1/instances") return json(200, { instances: opts.instances ?? [] });
    if (url === "/v1/settings/notify/overview") return opts.v28 ? json(200, v28Overview()) : err(404, "not_found", "no such route");
    if (url === "/v1/rules" && method === "GET") return json(200, { rules });
    const rm = /^\/v1\/rules\/([^/]+)\/mode$/.exec(url);
    if (rm && method === "PATCH") {
      const body = JSON.parse(init.body as string);
      patches.push({ url, body });
      const r = rules.find((x) => x.key === decodeURIComponent(rm[1]));
      if (!r) return err(404, "not_found", "no such rule");
      r.mode = body.mode;
      return json(200, r);
    }
    if (url === "/v1/deliveries/today") return json(200, { pushed: 7, cap: doc.disturb.daily_cap, suppressed_daily_cap: 0, suppressed_muted: 0 });
    if (url.startsWith("/v1/items/")) return json(200, { id: url.slice("/v1/items/".length), title: "Renew domain" });
    if (url === "/v1/agents") return opts.webOnlyDenied ? err(403, "web_only", "settings can only be used by the owner in the Web UI") : json(200, { agents: opts.agents ?? [], selected: {}, reason: "" });
    const keys = SECTIONS[url];
    if (!keys) return err(404, "not_found", "no such route");
    if (opts.webOnlyDenied) return err(403, "web_only", "settings can only be used by the owner in the Web UI");
    // disturb 照 api/settings/policy 的 disturbSection：总带生效的 delivery 与只读的 delivery_saved。
    const packs = Object.fromEntries(((opts.instances ?? []) as { id: string; pack: string }[]).map((i) => [i.id, i.pack]));
    const view = (k: string) => (k === "disturb" ? { ...doc.disturb, delivery: doc.disturb.delivery ?? deriveDelivery(doc.disturb, packs), delivery_saved: !!doc.disturb.delivery } : doc[k]);
    const render = () => ({ version, ...Object.fromEntries(keys.map((k) => [k, view(k)])) });
    if (method === "GET") return json(200, render());
    const body = JSON.parse(init.body as string);
    puts.push({ path: url, body, csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] });
    const apply = { ...body };
    if (body.disturb) {
      const { delivery_saved: _s, ...next } = body.disturb;
      if (!next.delivery && doc.disturb.delivery) next.delivery = doc.disturb.delivery; // 没带矩阵时沿用已存的
      const frozen = (x: any) => JSON.stringify([x.routes ?? {}, ["source_failing", "template_failed", "run_failed", "new_proposals"].map((c) => x.push?.[c]?.route ?? ""), x.proactive?.task_push ?? {}, x.proactive?.instance_push ?? {}]);
      if (next.delivery && frozen(next) !== frozen(doc.disturb)) return err(400, "invalid_request", "disturb routes, push.*.route, proactive.task_push and proactive.instance_push are frozen once disturb.delivery is saved");
      apply.disturb = next;
    }
    const before = JSON.stringify(doc);
    for (const [k, v] of Object.entries(apply) as [string, any][]) doc[k] = v;
    if (JSON.stringify(doc) !== before) version++;
    return json(200, render());
  }) as unknown as typeof fetch;
  return { impl, puts, doc, previews, patches, rules };
}
