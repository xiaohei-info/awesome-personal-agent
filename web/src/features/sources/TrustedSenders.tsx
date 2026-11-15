import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useLang, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { TagInput } from "../settings-policy/shared";
import { strings } from "./strings";
import type { Source } from "./shared";

// 来源 › 信任的发件人（§14.6、§4.1）：转发白名单里这个来源的那一个键。可不可信由来源程序声明（GET /sources 的 sender），
// 本人改不了。可信：标签输入，从未设置过时用 identity 模式预填本人（不自动保存）。需确认：只有一行说明；
// 以前填的条目本来就不生效，只在高级里给"清掉"（空名单，服务端对需确认来源只接受空名单）。
export function TrustedSenders({ source, add }: { source: Source; add?: string }) {
  const t = useT(strings);
  const lang = useLang();
  const { api } = useSession();
  const [stored, setStored] = useState<string[] | null | undefined>(undefined); // null：策略里没有这个键
  const [draft, setDraft] = useState<string[]>([]);
  const [prefilled, setPrefilled] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [saved, setSaved] = useState(false);
  const verified = source.sender?.trust === "verified";
  const hint = source.sender?.hint?.[lang] ?? "";
  const path = `/settings/sources/${encodeURIComponent(source.id)}/trusted-senders`;

  useEffect(() => {
    api.get<{ forward_allowlist?: Record<string, string[]> }>("/settings/allowlists").then(
      (r) => {
        const cur = r.forward_allowlist?.[source.id] ?? null;
        setStored(cur);
        setDraft([...new Set([...(cur ?? []), ...(add ? [add] : [])])]);
      },
      (e) => setError(e),
    );
  }, [api, source.id, add]);
  useEffect(() => {
    if (!verified || stored !== null || add || source.sender?.self !== "identity") return;
    // 推不出就留空并提示；不算错误
    api.get<{ sender: string }>(`/settings/sources/${encodeURIComponent(source.id)}/self-sender`).then(
      (r) => { if (r.sender) { setDraft([r.sender]); setPrefilled(true); } },
      () => undefined,
    );
  }, [api, verified, stored, add, source.id, source.sender?.self]);

  const put = async (list: string[]) => {
    setError(null);
    setSaved(false);
    try {
      const r = await api.put<{ senders: string[] }>(path, { senders: list });
      setStored(r.senders.length ? r.senders : null);
      setDraft(r.senders);
      setPrefilled(false);
      setSaved(true);
    } catch (e) {
      setError(e);
    }
  };

  if (stored === undefined) return error !== null ? <ErrorNote error={error} /> : null;
  return (
    <section id="trusted-senders" aria-label={t.trusted} className="stack">
      <h4>{t.trusted}</h4>
      {verified ? (
        <>
          <TagInput label={t.trusted} values={draft} placeholder={hint} onChange={(v) => { setDraft(v); setPrefilled(false); setSaved(false); }} />
          {prefilled ? <p className="sub">{t.prefilled}</p> : !draft.length && hint && <p className="sub">{t.fillYourself(hint)}</p>}
          <p className="sub">{t.trustedHint}</p>
          <div><button type="button" onClick={() => void put(draft)}>{t.save}</button>{saved && <span role="status"> {t.savedTrusted}</span>}</div>
        </>
      ) : (
        <>
          <p className="sub">{t.unverifiable}</p>
          {!!stored?.length && (
            <details>
              <summary>{t.advanced}</summary>
              <p className="sub">{t.legacy(stored.length)} <button type="button" className="link" onClick={() => void put([])}>{t.clear}</button></p>
            </details>
          )}
        </>
      )}
      {error !== null && <ErrorNote error={error} />}
    </section>
  );
}
