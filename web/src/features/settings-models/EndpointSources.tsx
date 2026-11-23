import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { OutboundEditor } from "../sources/Outbound";
import type { Source } from "../sources/shared";
import { CaptureOutbound, type Project } from "../projects/ProjectsPage";
import { strings } from "./strings";

// 模型端点能收哪些来源（§14.14）：来源内容与对话捕获送模型端点的范围只在这里改，来源页、项目页不再逐个配。
// 它只管内核调模型；外部程序与外部 Agent 读多少由签发钥匙时选的档位决定，APA 内部的 Agent 不受限。
export function EndpointSources() {
  const t = useT(strings);
  const { api } = useSession();
  const [sources, setSources] = useState<Source[] | null>(null);
  const [projects, setProjects] = useState<Project[]>([]);
  const [editing, setEditing] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const load = useCallback(async () => {
    try {
      setSources((await api.get<{ sources: Source[] }>("/sources")).sources);
    } catch (e) {
      setError(e);
    }
    api.get<{ projects: Project[] }>("/projects").then(
      (r) => setProjects(r.projects.filter((p) => p.capture)),
      () => setProjects([]),
    );
  }, [api]);
  useEffect(() => void load(), [load]);
  return (
    <section id="endpoint-sources" className="panel stack" aria-label={t.endpointSources}>
      <h2>{t.endpointSources}</h2>
      <p className="sub">{t.endpointSourcesHint}</p>
      {error !== null && <ErrorNote error={error} />}
      {sources === null ? (
        error === null && <p role="status">{t.loading}</p>
      ) : (
        <ul className="rows">
          {sources.map((s) => (
            <li key={s.id} aria-label={s.name}>
              <strong>{s.name}</strong>
              <OutboundEditor source={s} waiting={0} open={editing === s.id} onOpen={(o) => setEditing(o ? s.id : null)} onSaved={load} />
            </li>
          ))}
          {projects.map((p) => (
            <li key={p.id} aria-label={p.name}>
              <strong>{t.captureOf(p.name)}</strong>
              <CaptureOutbound key={p.id + (p.capture_outbound ?? "")} p={p} onSaved={() => void load()} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
