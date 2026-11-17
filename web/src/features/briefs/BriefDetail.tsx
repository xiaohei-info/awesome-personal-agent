import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { BriefBody, when, type Brief } from "./BriefBody";
import { strings } from "./strings";

/** /briefs/:id：一份简报的完整内容；刷新后按 id 重读账本。 */
export function BriefDetail() {
  const { id = "" } = useParams();
  const { api } = useSession();
  const t = useT(strings);
  const [b, setB] = useState<Brief | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    setB(null);
    setError(null);
    api.get<{ item: Brief }>("/items/" + encodeURIComponent(id)).then((r) => setB(r.item), setError);
  }, [api, id]);
  if (error != null) return <main><ErrorNote error={error} /></main>;
  if (!b) return <main><p role="status">{t.loading}</p></main>;
  return (
    <main>
      <p className="eyebrow"><Link to="..">{t.backToBriefs}</Link></p>
      <article aria-label={t.brief(b.title)}>
        <h1>{b.title}</h1>
        <p className="meta">{[b.spec?.period, when(b.created_at)].filter(Boolean).join(" · ")}</p>
        <BriefBody spec={b.spec} />
      </article>
    </main>
  );
}
