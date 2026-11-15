import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { PERM } from "../../app/ids";
import { Pick, Val, useLabel } from "../../app/values";
import { Card, PolicyPage, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { strings } from "./strings";

// 设置 › 后台权限：policy 的 default_harness 与 access-limits 的 permission_cap（附录 B.2）。两份白名单在来源详情与「设置 › 渠道」里编辑（§14.6）。
// 宿主列表与各宿主提供的档位取自已确认的无头运行声明（/settings/commands 的 headless.access，附录 A.2）；
// 文档里已有、但不在这些列表里的值保留为选项，不丢。
const ROUTES = ["policy", "access-limits"] as const;

export function SettingsAccessPage() {
  const t = useT(strings);
  const { docs, error, load, save } = usePolicyDocs(ROUTES);
  const pol = docs?.policy, lim = docs?.["access-limits"];
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!pol && !!lim} onRetry={() => void load()}>
      {pol && lim && (
        <>
          <HostCard doc={pol} save={save} />
          <LimitCard doc={lim} save={save} />
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
