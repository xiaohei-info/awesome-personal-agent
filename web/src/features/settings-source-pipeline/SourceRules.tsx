import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { useNames } from "../../app/names";
import { strings } from "./strings";

// 当前生效的来源规则（§9.3）：只读。GET /packs 的包列表与包内声明顺序就是匹配顺序；动作类规则是否生效看 GET /rules。
interface Rule {
  id: string;
  match?: Record<string, unknown>;
  where?: { field: string; op: string; value?: unknown }[];
  then: string;
  accept?: string;
  on_update?: string;
}
type Row = Rule & { pack: string };

const values = (v: unknown) => (Array.isArray(v) ? v : [v]).map(String);

export function SourceRules() {
  const t = useT(strings);
  const { api } = useSession();
  const sourceName = useNames("source");
  const [rows, setRows] = useState<Row[] | null>(null);
  const [live, setLive] = useState<Set<string>>(new Set());
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    api.get<{ packs?: { name: string; source_rules?: Rule[] }[] }>("/packs").then(
      (r) => setRows((r.packs ?? []).flatMap((p) => (p.source_rules ?? []).map((x) => ({ ...x, pack: p.name })))),
      () => setFailed(true),
    );
    api.get<{ rules?: { key: string; mode: string }[] }>("/rules").then(
      (r) => setLive(new Set((r.rules ?? []).filter((x) => x.mode === "auto").map((x) => x.key))),
      () => undefined,
    );
  }, [api]);

  const list = (vs: string[]) => vs.join(t.rules.or);
  const from = (m: Record<string, unknown>) => {
    const parts = [];
    if (m.source !== undefined) parts.push(list(values(m.source).map((v) => sourceName(v))));
    if (m.source_type !== undefined) parts.push(list(values(m.source_type).map((v) => t.rules.kinds[v] ?? t.rules.otherKind)));
    return parts.join(" · ") || t.rules.anySource;
  };
  const when = (r: Row) => {
    const m = r.match ?? {};
    const parts = [];
    if (m.event_type !== undefined) parts.push(list([...new Set(values(m.event_type).map((v) => t.rules.events[v] ?? t.rules.otherEvent))]));
    if (m.sender !== undefined) parts.push(t.rules.cond(t.rules.fields.sender, t.rules.ops.in, list(values(m.sender))));
    for (const c of r.where ?? []) {
      const field = c.field.startsWith("metrics.") ? t.rules.metric(c.field.slice("metrics.".length)) : (t.rules.fields[c.field] ?? t.rules.otherField);
      parts.push(t.rules.cond(field, t.rules.ops[c.op] ?? c.op, c.value === undefined ? "" : list(values(c.value))));
    }
    return parts.join(t.rules.and) || t.rules.anyMessage;
  };
  const exit = (r: Row) => {
    const on = live.has(`${r.pack}/source_rules/${r.id}`);
    const notes = [];
    if (r.then === "alert" && !on) notes.push(t.rules.alertOff);
    if (r.accept === "auto") notes.push(on ? t.rules.autoOn : t.rules.autoOff);
    if (r.on_update === "attach") notes.push(t.rules.attach);
    return (
      <>
        {t.rules.exits[r.then] ?? r.then}
        {notes.map((n) => <div key={n} className="sub">{n}</div>)}
      </>
    );
  };

  return (
    <section aria-label={t.rules.title} className="panel">
      <h2>{t.rules.title}</h2>
      <p className="sub">{t.rules.hint}</p>
      <p className="sub">{t.rules.where}</p>
      {rows === null ? (
        <p className="sub" role={failed ? "alert" : "status"}>{failed ? t.rules.failed : t.rules.loading}</p>
      ) : rows.length === 0 ? (
        <p className="sub">{t.rules.none}</p>
      ) : (
        <>
          <table>
            <thead>
              <tr><th>{t.rules.source}</th><th>{t.rules.condition}</th><th>{t.rules.outcome}</th></tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={`${r.pack}/${r.id}`}>
                  <td>{from(r.match ?? {})}</td>
                  <td>{when(r)}</td>
                  <td>{exit(r)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <details>
            <summary>{t.rules.raw}</summary>
            <ol>
              {rows.map(({ pack, id, ...r }) => (
                <li key={`${pack}/${id}`}><code>{`${pack}/${id} ${JSON.stringify(r)}`}</code></li>
              ))}
            </ol>
          </details>
        </>
      )}
    </section>
  );
}
