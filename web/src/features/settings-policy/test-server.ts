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

export function policyServer(opts: { webOnlyDenied?: boolean; agents?: Record<string, unknown>[]; connections?: { id: string; name: string; enabled: boolean }[]; routes?: Record<string, string>; extKeys?: Record<string, unknown>[] } = {}) {
  let version = 1;
  const doc = initialDoc();
  if (opts.routes) doc.disturb.routes = opts.routes;
  const puts: { path: string; body: any; csrf?: string }[] = [];
  const tiers: { url: string; body: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [{ id: "acc-home", name: "Home" }] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_cal_apple", name: "Apple Calendar" }, { id: "src_mail_qq", name: "QQ Mail" }] });
    if (url === "/v1/settings/ext-keys") return json(200, { credentials: opts.extKeys ?? [{ id: "k1", kind: "ext", scope: "channel.slack" }, { id: "k2", kind: "ext", scope: "channel.old", revoked_at: "x" }] });
    if (url === "/v1/connections?direction=channel") return json(200, { connections: opts.connections ?? [] });
    if (url === "/v1/settings/commands") return json(200, { declarations: [{ harness: "hermes", rev: 1, state: "confirmed", declaration: { harness: "hermes", headless: { command: ["hermes"], access: { read: [], edit: [] } } } }] });
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
    const render = () => ({ version, ...Object.fromEntries(keys.map((k) => [k, doc[k]])) });
    if (method === "GET") return json(200, render());
    const body = JSON.parse(init.body as string);
    puts.push({ path: url, body, csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] });
    if (typeof body.persona === "string" && body.persona.length > 400) return err(400, "invalid_request", "persona is about 500 tokens, limit 400");
    const before = JSON.stringify(doc);
    for (const [k, v] of Object.entries(body) as [string, any][]) doc[k] = v;
    if (JSON.stringify(doc) !== before) version++;
    return json(200, render());
  }) as unknown as typeof fetch;
  return { impl, puts, doc, tiers };
}
