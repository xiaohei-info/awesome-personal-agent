import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { locale, useLang, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useItemTitle } from "../../app/names";
import { useRaw } from "../../app/raw";
import { entryOf } from "../../app/values";
import { RULE_MODE } from "../../app/ids";
import { strings } from "./strings";

// 形状对应 GET /v1/rules（含 class）、PATCH /v1/rules/{id}/mode（附录 B.2）、GET /v1/packs 与 GET /v1/audit/events（§14.8）。
// 谁能切、切到什么、观察记录怎么产生都由服务端判定；本页只搬运选择，被拒绝时模式保持原值。只改模式，不编辑规则内容。
interface Rule { key: string; pack: string; group: string; id: string; mode: string; class?: "reminder" | "action" }
interface Pack { name: string; description?: string }
interface Observed { seq: number; object: string; at: string; reason?: string; payload?: { would_do?: unknown } }

const WEEK = 7 * 24 * 3600 * 1000;
const observed = (key: string) => "/audit/events?" + new URLSearchParams({ type: "rule.observed", actor: "rule:" + key, limit: "500" });

function Line({ e }: { e: Observed }) {
  const t = useT(strings);
  const raw = useRaw();
  const title = useItemTitle(e.object);
  return (
    <li>
      {t.observed(title, new Date(e.at).toLocaleString(locale()))}
      {raw && <code className="raw-id">{`#${e.seq} ${e.reason ?? ""} ${JSON.stringify(e.payload?.would_do ?? null)}`}</code>}
    </li>
  );
}

/** 试运行的规则：本周本来会做几次，点开是一句话列表。 */
function Trial({ rule, name }: { rule: Rule; name: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [events, setEvents] = useState<Observed[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [open, setOpen] = useState(false);
  useEffect(() => {
    api.get<{ events: Observed[] }>(observed(rule.key)).then((r) => setEvents(r.events), setError);
  }, [api, rule.key]);
  if (error != null) return <ErrorNote error={error} />;
  if (!events) return null;
  const week = events.filter((e) => Date.now() - new Date(e.at).getTime() < WEEK);
  return (
    <>
      <button type="button" className="quiet" aria-expanded={open} disabled={!week.length} onClick={() => setOpen(!open)}>
        {open ? t.hide : t.week(week.length)}
      </button>
      {open && (
        <ul aria-label={t.observations(name)} className="sub-list">
          {[...week].reverse().map((e) => <Line key={e.seq} e={e} />)}
        </ul>
      )}
    </>
  );
}

function Modes({ id, name, value, onChange }: { id: string; name: string; value: string; onChange: (m: string) => void }) {
  const lang = useLang();
  const t = useT(strings);
  return (
    <fieldset className="seg-radio" role="radiogroup" aria-label={t.mode(name)}>
      {RULE_MODE.map((m) => (
        <label key={m}>
          <input type="radio" name={"mode-" + id} value={m} checked={value === m} onChange={() => onChange(m)} />
          {entryOf(lang, "ruleMode", m)?.label ?? m}
        </label>
      ))}
    </fieldset>
  );
}

export function RulesPage() {
  const { api } = useSession();
  const lang = useLang();
  const raw = useRaw();
  const t = useT(strings);
  const [rules, setRules] = useState<Rule[] | null>(null);
  const [packs, setPacks] = useState<Pack[]>([]);
  const [error, setError] = useState<unknown>(null);

  const load = useCallback(async () => {
    try {
      setRules((await api.get<{ rules: Rule[] }>("/rules")).rules);
      setError(null);
    } catch (e) {
      setRules(null);
      setError(e);
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => { api.get<{ packs: Pack[] }>("/packs").then((r) => setPacks(r.packs), () => setPacks([])); }, [api]);

  async function setMode(r: Rule, mode: string) {
    try {
      const next = await api.patch<Rule>(`/rules/${encodeURIComponent(r.key)}/mode`, { mode });
      setRules((l) => l!.map((x) => (x.key === r.key ? { ...x, ...next } : x)));
      setError(null);
    } catch (e) {
      setError(e); // 单选的值仍是 r.mode，即服务端未改动的原模式
    }
  }
  // 规则名：词表按「包/规则 id」查；查不到时用包的说明。
  const nameOf = (r: Rule) => entryOf(lang, "rule", `${r.pack}/${r.id}`)?.label ?? packs.find((p) => p.name === r.pack)?.description ?? r.id;
  const groups = (["reminder", "action", "other"] as const)
    .map((g) => ({ g, list: (rules ?? []).filter((r) => (r.class ?? "other") === g) }))
    .filter((x) => x.list.length);

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} />}
      {!rules && error == null && <p role="status">{t.loading}</p>}
      {rules && !rules.length && <p className="empty">{t.none}</p>}
      {groups.map(({ g, list }) => (
        <section key={g} aria-label={t.groups[g]} className="panel">
          <h2>{t.groups[g]}</h2>
          <ul className="rows">
            {list.map((r) => {
              const name = nameOf(r);
              return (
                <li key={r.key} className="rule">
                  <div>
                    <strong>{name}</strong>
                    <p className="sub">
                      {entryOf(lang, "ruleGroup", r.group)?.label ?? r.group}
                      {raw && <code className="raw-id">{r.key}</code>}
                    </p>
                    {r.mode === "observe" && <Trial rule={r} name={name} />}
                  </div>
                  <Modes id={r.key} name={name} value={r.mode} onChange={(m) => void setMode(r, m)} />
                </li>
              );
            })}
          </ul>
          {g !== "reminder" && <p className="sub">{t.hint}</p>}
        </section>
      ))}
    </main>
  );
}
