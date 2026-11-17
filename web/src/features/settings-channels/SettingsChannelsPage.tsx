import { useCallback, useEffect, useState } from "react";
import type { ApiClient } from "../../app/api";
import { useSession } from "../../app/session";
import { errorText as msg, useT } from "../../app/i18n";
import { Card, PolicyPage, TagInput, useDraft, usePolicyDocs, type Doc } from "../settings-policy/shared";
import { strings } from "./strings";
import { Connections, time } from "./Connections";

// 设置 › 提醒渠道（§4.1、§14.6）。上半是内置渠道连接（Connections.tsx）；底部折叠的「用自己的程序接入」是 ext 渠道：
// 没有独立实体，名字取自 scope 为 channel.<名> 的 ext 密钥，属性在策略的 channels 段，回答人在 channel_answerers。回答人只经 PUT /settings/channels/{名}/answerers 改这一个键；
// "识别我的 ID"只在内存里，本人点"加上"才写名单。web_only、回答人校验都在服务端。

interface Key { id: string; label?: string; scope?: string; revoked_at?: string; last_seen_at?: string }
const CH = "channel.";
const ROUTES = ["policy", "allowlists"] as const;

async function extKeys(api: ApiClient): Promise<Key[]> {
  return (await api.get<{ credentials: Key[] }>("/settings/ext-keys")).credentials.filter((k) => k.scope?.startsWith(CH));
}

/** 已接入的渠道：内置渠道连接与没吊销的渠道密钥。 */
export async function channelNames(api: ApiClient): Promise<string[]> {
  const [conns, keys] = await Promise.all([
    api.get<{ connections: { name: string }[] }>("/connections?direction=channel").then((r) => r.connections, () => []),
    extKeys(api),
  ]);
  return [...conns.map((c) => c.name), ...new Set(keys.filter((k) => !k.revoked_at).map((k) => k.scope!.slice(CH.length)))];
}

export function SettingsChannelsPage() {
  const t = useT(strings);
  const { api } = useSession();
  const { docs, error, load, save } = usePolicyDocs(ROUTES);
  const [keys, setKeys] = useState<Key[]>([]);
  const [secret, setSecret] = useState<string | null>(null);
  const loadKeys = useCallback(() => extKeys(api).then(setKeys, () => setKeys([])), [api]);
  useEffect(() => void loadKeys(), [loadKeys]);
  const pol = docs?.policy, al = docs?.allowlists;
  const names = [...new Set([
    ...keys.filter((k) => !k.revoked_at).map((k) => k.scope!.slice(CH.length)),
    ...Object.keys(pol?.channels ?? {}),
    ...Object.keys(al?.channel_answerers ?? {}),
  ])];
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!pol && !!al} onRetry={() => void load()}>
      {secret && (
        <div role="alert" className="secret">
          <p>{t.secretOnce}</p>
          <code>{secret}</code>
          <div><button onClick={() => setSecret(null)}>{t.savedIt}</button></div>
        </div>
      )}
      {pol && (
        <Connections routes={pol.disturb?.routes ?? {}}
          saveRoutes={(routes) => save("policy", { disturb: { ...pol.disturb, routes } })} onRoutesChanged={() => void load()} />
      )}
      <details open={names.length > 0}>
        <summary>{t.ownProgram}</summary>
        <div className="stack">
          {pol && al && names.map((n) => (
            <ChannelCard key={n} name={n} pol={pol} al={al} keys={keys.filter((k) => k.scope === CH + n)}
              onSaved={() => void load()} onKeys={() => void loadKeys()}
              savePolicy={(body) => save("policy", body)} />
          ))}
          <NewChannel onSecret={(s) => { setSecret(s); void loadKeys(); }} />
        </div>
      </details>
    </PolicyPage>
  );
}

function ChannelCard({ name, pol, al, keys, onSaved, onKeys, savePolicy }: {
  name: string; pol: Doc; al: Doc; keys: Key[]; onSaved: () => void; onKeys: () => void; savePolicy: (body: unknown) => Promise<number>;
}) {
  const t = useT(strings);
  const { api } = useSession();
  const stored = (pol.channels ?? {}) as Record<string, { text_only?: boolean }>;
  const storedAns = ((al.channel_answerers ?? {}) as Record<string, string[]>)[name] ?? [];
  const [textOnly, setTextOnly] = useDraft(pol.channels, () => !!stored[name]?.text_only);
  const [ans, setAns] = useDraft(al.channel_answerers, () => storedAns);
  // 只签了钥匙不算接通：按钥匙最近一次真实调用内核的时间区分"还没连上过 / 最近使用"。
  const seen = keys.filter((k) => !k.revoked_at && k.last_seen_at).map((k) => k.last_seen_at!).sort().at(-1);
  const putAnswerers = (list: string[]) => api.put<{ version: number }>(`/settings/channels/${encodeURIComponent(name)}/answerers`, { answerers: list });
  const submit = async () => {
    let v = (await putAnswerers(ans)).version;
    if (textOnly !== !!stored[name]?.text_only) v = await savePolicy({ channels: { ...stored, [name]: { text_only: textOnly } } });
    onSaved();
    return v;
  };
  const revoke = async (id: string) => {
    await api.del(`/settings/ext-keys/${encodeURIComponent(id)}`).catch(() => undefined);
    onKeys(); // 以服务端状态为准
  };
  return (
    <Card title={name} onSave={submit}>
      <p className="sub" aria-label={t.linkState}>{seen ? t.lastUsed(time(seen)) : t.neverConnected}</p>
      <label><input type="checkbox" checked={textOnly} onChange={(e) => setTextOnly(e.target.checked)} /> {t.textOnly}</label>
      {textOnly && <p className="sub">{t.textOnlyHint}</p>}
      <h3>{t.answerers}</h3>
      <TagInput label={t.answerers} values={ans} placeholder={t.answerersHint} onChange={setAns} />
      <Identify channel={name} onAdd={async (id) => { await putAnswerers([...new Set([...storedAns, id])]); onSaved(); }} />
      {keys.length > 0 && (
        <details>
          <summary>{t.advanced}</summary>
          <h3>{t.keys}</h3>
          <ul className="rows">
            {keys.map((k) => (
              <li key={k.id}>
                <strong>{k.label || k.id}</strong>{" "}
                {k.revoked_at ? <span className="badge">{t.revoked}</span> : <button type="button" className="danger" onClick={() => void revoke(k.id)}>{t.revoke}</button>}
              </li>
            ))}
          </ul>
        </details>
      )}
    </Card>
  );
}

interface Ident { state: "waiting" | "got" | "expired"; sender?: string; display?: string }
export const POLL_MS = 2000;

/** 识别我的 ID：发起 → 给机器人发口令 → 轮询结果 → 本人点「加上」才写名单。 */
function Identify({ channel, onAdd }: { channel: string; onAdd: (id: string) => Promise<void> }) {
  const t = useT(strings);
  const { api } = useSession();
  const [code, setCode] = useState<string | null>(null);
  const [st, setSt] = useState<Ident | null>(null);
  const [error, setError] = useState<string | null>(null);
  const base = `/settings/channels/${encodeURIComponent(channel)}/identify`;
  useEffect(() => {
    if (!code || st?.state === "got" || st?.state === "expired") return;
    let stop = false;
    const tick = () =>
      api.get<Ident>(base).then(
        (r) => !stop && setSt(r),
        (e) => !stop && setError(msg(e)),
      );
    void tick();
    const h = setInterval(tick, POLL_MS);
    return () => { stop = true; clearInterval(h); };
  }, [api, base, code, st?.state]);
  const start = async () => {
    setError(null);
    setSt(null);
    try {
      setCode((await api.post<{ code: string }>(base, {})).code);
    } catch (e) {
      setError(msg(e));
    }
  };
  const add = async (id: string) => {
    setError(null);
    try {
      await onAdd(id);
      setCode(null);
      setSt(null);
    } catch (e) {
      setError(msg(e));
    }
  };
  return (
    <div role="group" aria-label={t.identify}>
      {!code || st?.state === "expired" ? (
        <>
          {st?.state === "expired" && <span className="sub">{t.expired} </span>}
          <button type="button" onClick={() => void start()}>{st?.state === "expired" ? t.retry : t.identify}</button>
        </>
      ) : st?.state === "got" && st.sender ? (
        <p>{t.got(st.display ?? "", st.sender)} <button type="button" className="primary" onClick={() => void add(st.sender!)}>{t.addIt}</button></p>
      ) : (
        <>
          <p><code>{t.sendThis(code)}</code></p>
          <p className="sub" role="status">{t.waiting}</p>
        </>
      )}
      {error && <p role="alert">{error}</p>}
    </div>
  );
}

/** 开发者入口：给自己写的渠道程序签一把 scope 为 channel.<名> 的 ext 密钥；明文只显示一次。签了不等于接通，卡片按真实调用显示状态。 */
function NewChannel({ onSecret }: { onSecret: (s: string) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const create = async () => {
    setError(null);
    try {
      const r = await api.post<{ secret: string }>("/settings/ext-keys", { scope: CH + name.trim() });
      setName("");
      onSecret(r.secret);
    } catch (e) {
      setError(msg(e));
    }
  };
  return (
    <form className="panel" aria-label={t.newChannel} onSubmit={(e) => { e.preventDefault(); void create(); }}>
      <h2>{t.newChannel}</h2>
      <label>{t.channelName} <input value={name} onChange={(e) => setName(e.target.value)} /></label>{" "}
      <button type="submit" disabled={!name.trim()}>{t.create}</button>
      {error && <p role="alert">{error}</p>}
    </form>
  );
}
