import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { HostedMaterial } from "./HostedMaterial";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 项目页的材料与规范视图：只显示该项目的数据（服务端按 project 过滤），托管型与自有型并列并标明来源。
interface Mat {
  id: string;
  project: string;
  kind: string;
  title: string;
  location?: string;
  current_rev?: string;
}


export function ProjectMaterials({ project }: { project: string }) {
  const { api } = useSession();
  const t = useT(strings);
  const [mats, setMats] = useState<Mat[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<Mat | null>(null);

  useEffect(() => {
    setMats(null);
    setOpen(null);
    api.get<{ materials: Mat[] }>(`/materials?project=${encodeURIComponent(project)}`).then((r) => setMats(r.materials), (e) => setError(msg(e)));
  }, [api, project]);

  // 规范只存在自有型（Guideline 材料），其余并列显示。
  const guidelines = mats?.filter((m) => m.kind === "guideline" && !m.location) ?? [];
  const rest = mats?.filter((m) => !(m.kind === "guideline" && !m.location)) ?? [];

  return (
    <>
      <section aria-label={t.materials}>
        <h2>{t.materials}</h2>
        {error && <p role="alert">{error}</p>}
        {mats && !rest.length && !error && <p className="sub">{t.noMaterials}</p>}
        {!!rest.length && (
          <ul className="rows">
            {rest.map((m) => (
              <li key={m.id}>
                <button className="link" onClick={() => setOpen(m)}>{m.title}</button> <span className="sub">{m.kind} · {m.location ? t.hosted : t.owned}
                {m.location && <> · {m.location}</>}</span>
              </li>
            ))}
          </ul>
        )}
        {open?.location && <HostedMaterial key={open.id} material={open} />}
      </section>
      <section aria-label={t.guidelines}>
        <h2>{t.guidelines}</h2>
        {mats && !guidelines.length && !error && <p className="sub">{t.noGuidelines}</p>}
        {guidelines.map((g) => (
          <Guideline key={g.id} m={g} />
        ))}
      </section>
    </>
  );
}

function Guideline({ m }: { m: Mat }) {
  const { api } = useSession();
  const t = useT(strings);
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    api.get<{ revision: { content: string } | null }>(`/materials/${encodeURIComponent(m.id)}`).then(
      (r) => setText(r.revision?.content ?? ""),
      (e) => setError(msg(e)),
    );
  }, [api, m.id]);
  return (
    <article aria-label={m.title}>
      <h3>{m.title}</h3>
      {error && <p role="alert">{error}</p>}
      {text === "" && <p className="sub">{t.noActiveRevision}</p>}
      {text && <pre>{text}</pre>}
    </article>
  );
}
