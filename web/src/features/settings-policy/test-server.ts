// Test double for the settings pages that edit the policy document (not shipped: only imported by tests).
// It answers like api/settings/policy: three routes read and replace whole sections, keys are masked,
// an over-long persona is rejected and nothing is saved, GET /agents lists the Agent cards with their tier,
// and a non-Web session gets 403 web_only (error bodies follow appendix B.3).
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix it; nothing was saved" });

export const SECTIONS: Record<string, string[]> = {
  "/v1/settings/policy": ["disturb", "persona", "persona_lang", "channels", "default_harness", "main_session_launch"],
  "/v1/settings/allowlists": ["forward_allowlist", "channel_answerers"],
  "/v1/settings/access-limits": ["permission_cap"],
};

export function initialDoc(): Record<string, any> {
  return {
    disturb: { routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 20, mutes: [] },
    persona: "Be brief.",
    persona_lang: "",
    channels: {},
    default_harness: "",
    main_session_launch: { by_harness: {}, by_rule: {} },
    forward_allowlist: { telegram: ["alice"] },
    channel_answerers: {},
    permission_cap: { default: "read", by_harness: {} },
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

export function policyServer(opts: { preview?: Record<string, unknown>; instances?: Record<string, unknown>[]; webOnlyDenied?: boolean; agents?: Record<string, unknown>[]; connections?: { id: string; name: string; enabled: boolean }[]; routes?: Record<string, string>; extKeys?: Record<string, unknown>[] } = {}) {
  let version = 1;
  const doc = initialDoc();
  if (opts.routes) doc.disturb.routes = opts.routes;
  const puts: { path: string; body: any; csrf?: string }[] = [];
  const tiers: { url: string; body: any }[] = [];
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
    if (url === "/v1/deliveries/today") return json(200, { pushed: 7, cap: doc.disturb.daily_cap, suppressed_daily_cap: 0, suppressed_muted: 0 });
    if (url.startsWith("/v1/items/")) return json(200, { id: url.slice("/v1/items/".length), title: "Renew domain" });
    if (url.startsWith("/v1/context/persona")) return json(200, { persona: "decl\n" + doc.persona, default: (doc.persona_lang || (url.endsWith("locale=zh") ? "zh" : "en")) === "zh" ? "Default tone (zh)" : "Default tone (en)" });
    if (url.endsWith("/tier") && method === "PUT") {
      tiers.push({ url, body: JSON.parse(init.body as string) });
      return json(200, { version: ++version });
    }
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
    if (typeof body.persona === "string" && body.persona.length > 400) return err(400, "invalid_request", "persona is about 500 tokens, limit 400");
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
  return { impl, puts, doc, tiers, previews };
}
