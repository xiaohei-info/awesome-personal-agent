import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { PERM } from "../../app/ids";
import { useNamed } from "../../app/names";
import { Pick, Val, useLabel } from "../../app/values";
import { Card, PolicyPage, TagInput, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { strings } from "./strings";

// 设置 › 后台权限：policy 的 default_harness、access-limits 的 permission_cap 与 allowlists 的两份白名单（附录 B.2）。
// 宿主列表与各宿主提供的档位取自已确认的无头运行声明（/settings/commands 的 headless.access，附录 A.2）；
// 渠道取自渠道扩展的密钥（scope channel.<名>）。文档里已有、但不在这些列表里的值保留为选项，不丢。
const ROUTES = ["policy", "access-limits", "allowlists"] as const;

export function SettingsAccessPage() {
  const t = useT(strings);
  const { docs, error, load, save } = usePolicyDocs(ROUTES);
  const pol = docs?.policy, lim = docs?.["access-limits"], al = docs?.allowlists;
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!pol && !!lim && !!al} onRetry={() => void load()}>
      {pol && lim && al && (
        <>
          <HostCard doc={pol} save={save} />
          <LimitCard doc={lim} save={save} />
          <AllowlistCard doc={al} save={save} />
        </>
      )}
    </PolicyPage>
  );
}

/** 已确认声明的宿主 → 它声明了的档位。 */
function useHosts(): Record<string, string[]> {
  const { api } = useSession();
  const [hosts, setHosts] = useState<Record<string, string[]>>({});
  useEffect(() => {
    api.get<{ declarations: { harness: string; declaration: { headless?: { access?: Record<string, unknown> } } }[] }>("/settings/commands").then(
      (r) => setHosts(Object.fromEntries(r.declarations.map((d) => [d.harness, Object.keys(d.declaration.headless?.access ?? {})]))),
      () => setHosts({}), // 只用来列选项，取不到就只显示已设的
    );
  }, [api]);
  return hosts;
}

// 规则没指定宿主时，任务型与后台运行用这个宿主；不设时这类运行不启动，事项转为等你决定（§13.6）。
// PUT /settings/policy 按段替换，只发 default_harness 这一段，其余策略不动。
function HostCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const hosts = useHosts();
  const [host, setHost] = useDraft(doc.default_harness, () => (doc.default_harness as string) ?? "");
  return (
    <Card title={t.defaultHost} hint={host ? undefined : t.noHostHint} onSave={() => save("policy", { default_harness: host })}>
      <select aria-label={t.defaultHost} value={host} onChange={(e) => setHost(e.target.value)}>
        <option value="">{t.noHost}</option>
        {withCurrent(Object.keys(hosts), host).map((x) => <option key={x} value={x}>{x}</option>)}
      </select>
    </Card>
  );
}

function useChannels(): string[] {
  const { api } = useSession();
  const [names, setNames] = useState<string[]>([]);
  useEffect(() => {
    api.get<{ credentials: { scope?: string; revoked_at?: string }[] }>("/settings/ext-keys").then(
      (r) => setNames([...new Set(r.credentials.filter((c) => !c.revoked_at && c.scope?.startsWith("channel.")).map((c) => c.scope!.slice("channel.".length)))]),
      () => setNames([]),
    );
  }, [api]);
  return names;
}

const withCurrent = (list: string[], cur: string) => (cur && !list.includes(cur) ? [...list, cur] : list);

function LimitCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const label = useLabel();
  const hosts = useHosts();
  const [def, setDef] = useDraft(doc.permission_cap, () => doc.permission_cap.default as string);
  const [rows, setRows] = useDraft(doc.permission_cap, () => Object.entries((doc.permission_cap.by_harness ?? {}) as Record<string, string>));
  const set = (i: number, j: 0 | 1, v: string) => setRows(rows.map((r, n) => (n === i ? ((j ? [r[0], v] : [v, r[1]]) as [string, string]) : r)));
  const submit = () => {
    const by: Record<string, string> = {};
    for (const [h, v] of rows) {
      if (!h) continue;
      if (h in by) return reject(t.dup(h));
      by[h] = v;
    }
    return save("access-limits", { permission_cap: { default: def, by_harness: by } });
  };
  return (
    <Card title={t.cap} hint={t.capHint} onSave={submit}>
      <fieldset className="cards3">
        <legend>{t.defaultCap}</legend>
        {PERM.map((p) => (
          <label key={p}>
            <span>
              <input type="radio" name="perm-default" value={p} aria-label={label("perm", p)} checked={def === p} onChange={() => setDef(p)} /> <b><Val k="perm" id={p} /></b>
            </span>
            <ul>{t.can[p].map((x) => <li key={x}>{x}</li>)}</ul>
          </label>
        ))}
      </fieldset>
      <h3>{t.byHost}</h3>
      {rows.map(([h, v], i) => {
        const offered = hosts[h];
        return (
          <div key={i} className="line">
            <span>
              <select aria-label={t.host(i + 1)} value={h} onChange={(e) => set(i, 0, e.target.value)}>
                <option value="">{t.pick}</option>
                {withCurrent(Object.keys(hosts), h).map((x) => <option key={x} value={x}>{x}</option>)}
              </select>
              {h && <small>{offered ? t.offers(offered.map((p) => label("perm", p)).join(t.listSep)) : t.notDeclared}</small>}
            </span>
            <Pick k="perm" label={t.hostLevel(i + 1)} value={v} ids={withCurrent(offered?.length ? offered : [...PERM], v)} onChange={(x) => set(i, 1, x)} />
            <button type="button" aria-label={t.delHost(i + 1)} onClick={() => setRows(rows.filter((_, n) => n !== i))}>✕</button>
          </div>
        );
      })}
      <div>
        <button type="button" className="link" onClick={() => setRows([...rows, [Object.keys(hosts).find((x) => !rows.some((r) => r[0] === x)) ?? "", "read"]])}>{t.addHost}</button>
      </div>
    </Card>
  );
}

type ListRow = [key: string, values: string[]];

// 两份白名单在同一个 PUT 里保存（与改版前同一请求体）；只是分成两块显示。
function AllowlistCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const sources = useNamed("source");
  const channels = useChannels();
  const [fwd, setFwd] = useDraft(doc.forward_allowlist, () => Object.entries((doc.forward_allowlist ?? {}) as Record<string, string[]>) as ListRow[]);
  const [ans, setAns] = useDraft(doc.channel_answerers, () => Object.entries((doc.channel_answerers ?? {}) as Record<string, string[]>) as ListRow[]);
  const toMap = (rows: ListRow[]) => {
    const out: Record<string, string[]> = {};
    for (const [k, v] of rows) {
      if (!k && !v.length) continue;
      if (k in out) throw new Error(t.dup(k));
      out[k] = v;
    }
    return out;
  };
  const submit = async () => save("allowlists", { forward_allowlist: toMap(fwd), channel_answerers: toMap(ans) });
  // 转发白名单的键是来源或渠道（§14.6）：两者都列出。
  const fwdKeys = [...sources.map((s) => [s.id, s.name] as const), ...channels.map((c) => [c, c] as const)];
  const edit = (rows: ListRow[], setRows: (r: ListRow[]) => void, i: number, row: ListRow) => setRows(rows.map((r, n) => (n === i ? row : r)));
  return (
    <Card title={t.lists} onSave={submit}>
      <h3>{t.forward}</h3>
      {fwd.map(([k, v], i) => (
        <div key={i} className="line">
          <select aria-label={t.sourceN(i + 1)} value={k} onChange={(e) => edit(fwd, setFwd, i, [e.target.value, v])}>
            <option value="">{t.pick}</option>
            {[...fwdKeys, ...(k && !fwdKeys.some(([id]) => id === k) ? [[k, k] as const] : [])].map(([id, name]) => <option key={id} value={id}>{name}</option>)}
          </select>
          <TagInput label={t.senders(i + 1)} values={v} onChange={(x) => edit(fwd, setFwd, i, [k, x])} />
          <button type="button" aria-label={t.delRow(i + 1)} onClick={() => setFwd(fwd.filter((_, n) => n !== i))}>✕</button>
        </div>
      ))}
      <div><button type="button" className="link" onClick={() => setFwd([...fwd, ["", []]])}>{t.addSource}</button></div>
      <p className="sub">{t.forwardHint}</p>
      <h3>{t.answerers}</h3>
      {ans.map(([k, v], i) => (
        <div key={i} className="line">
          <select aria-label={t.channelN(i + 1)} value={k} onChange={(e) => edit(ans, setAns, i, [e.target.value, v])}>
            <option value="">{t.pick}</option>
            {withCurrent(channels, k).map((c) => <option key={c} value={c}>{c}</option>)}
          </select>
          <TagInput label={t.people(i + 1)} values={v} onChange={(x) => edit(ans, setAns, i, [k, x])} />
          <button type="button" aria-label={t.delRow(i + 1)} onClick={() => setAns(ans.filter((_, n) => n !== i))}>✕</button>
        </div>
      ))}
      <div><button type="button" className="link" onClick={() => setAns([...ans, [channels[0] ?? "", []]])}>{t.addChannel}</button></div>
    </Card>
  );
}
