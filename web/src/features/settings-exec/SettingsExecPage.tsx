import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";
import { useLabel } from "../../app/values";

// 形状对应 /v1/settings/{declarations,commands}（kernel/executors）。授权（仅 Web 会话）、
// 声明校验、确认前不生效都在服务端；本页只负责调用并如实展示服务端的回答。
// 执行者由宿主在委派时登记，不在设置里手填；谁在干活见"agent 动态 › 执行者"（§6.17）。
interface Executor {
  id: string;
  kind: string;
  host?: string;
  scenarios?: string[];
}
interface Declaration {
  harness: string;
  headless?: { command?: string[] | string; access?: Record<string, unknown> } & Record<string, unknown>;
  selfcheck?: { command?: unknown };
  main_session?: Record<string, unknown>;
  executors?: Executor[];
}
interface Record_ {
  harness: string;
  rev: number;
  state: string;
  declaration: Declaration;
}


// 每个分区各自加载、各自报错；写操作之后一律重新读取，以服务端状态为准。
function useList<T>(path: string, pick: (r: any) => T[]) {
  const { api } = useSession();
  const [items, setItems] = useState<T[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(
    () =>
      api.get<unknown>(path).then(
        (r) => setItems(pick(r)),
        (e) => setError(msg(e)),
      ),
    [api, path], // eslint-disable-line react-hooks/exhaustive-deps
  );
  useEffect(() => {
    load();
  }, [load]);
  return { items, error, setError, load };
}

// 完整、只读地展示整份声明：确认/登记的就是这里显示的全部内容（命令、access、resume、main_session、执行者）。
// 先给一行摘要（要运行的命令、提供的权限档、附带的执行者），完整声明收在下面可展开，批准前可以逐字看。
const DeclarationView = ({ d }: { d: Declaration }) => {
  const t = useT(strings);
  const label = useLabel();
  const cmd = d.headless?.command;
  const levels = Object.keys(d.headless?.access ?? {});
  return (
    <div className="full">
      {cmd && (
        <p>
          {t.wantsToRun(d.harness)}
          <code>{Array.isArray(cmd) ? cmd.join(" ") : String(cmd)}</code>
        </p>
      )}
      <p className="sub">
        {[levels.length ? t.levels(levels.map((p) => label("perm", p)).join(t.listSep)) : "", d.executors?.length ? t.withExecutors(d.executors.length) : "", d.selfcheck ? t.selfcheck : ""].filter(Boolean).join(" · ")}
      </p>
      <details>
        <summary>{t.fullDeclaration}</summary>
        <pre data-testid={`declaration-${d.harness}`}>{JSON.stringify(d, null, 2)}</pre>
      </details>
    </div>
  );
};

function PendingSection({ onChanged }: { onChanged: () => void }) {
  const { api } = useSession();
  const t = useT(strings);
  const { items, error, setError, load } = useList<Record_>("/settings/declarations", (r) => r.declarations);

  const decide = async (r: Record_, verb: "confirm" | "reject") => {
    setError(null);
    try {
      await api.post(`/settings/declarations/${encodeURIComponent(r.harness)}/${r.rev}/${verb}`);
    } catch (e) {
      setError(msg(e));
    }
    await load();
    onChanged();
  };

  return (
    <section role="region" aria-labelledby="h-pending">
      <h2 id="h-pending">{t.pending}</h2>
      <p className="sub">{t.pendingHint}</p>
      {error && <div role="alert">{error}</div>}
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noPending}</p>
      ) : (
        <ul className="rows">
          {items.map((r) => (
            <li key={`${r.harness}/${r.rev}`}>
              <span>
                <strong>{r.harness}</strong>
              </span>
              <DeclarationView d={r.declaration} />
              <div className="actions">
                <button className="danger" onClick={() => decide(r, "confirm")}>{t.confirm}</button> <button onClick={() => decide(r, "reject")}>{t.reject}</button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function CommandsSection({ reloadKey }: { reloadKey: number }) {
  const { api } = useSession();
  const t = useT(strings);
  const { items, error, setError, load } = useList<Record_>("/settings/commands", (r) => r.declarations);
  const [text, setText] = useState("");

  useEffect(() => {
    if (reloadKey) load();
  }, [reloadKey, load]);

  const register = async () => {
    setError(null);
    let body: unknown;
    try {
      body = JSON.parse(text);
    } catch {
      setError(t.badJSON);
      return;
    }
    try {
      await api.post("/settings/commands", body);
      setText("");
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };

  return (
    <section role="region" aria-labelledby="h-commands">
      <h2 id="h-commands">{t.commands}</h2>
      {error && <div role="alert">{error}</div>}
      <details className="panel">
        <summary>{t.advanced}</summary>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            register();
          }}
        >
          <label>
            {t.declaration}
            <textarea rows={8} value={text} onChange={(e) => setText(e.target.value)} />
          </label>
          <div>
            <button type="submit" disabled={!text.trim()}>{t.register}</button>
          </div>
        </form>
      </details>
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noCommands}</p>
      ) : (
        <ul className="rows">
          {items.map((r) => (
            <li key={`${r.harness}/${r.rev}`}>
              <span>
                <strong>{r.harness}</strong>
              </span>
              <DeclarationView d={r.declaration} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export function SettingsExecPage() {
  // 确认/拒绝声明会改变已允许的命令；用计数通知命令分区重新读取。
  const [n, setN] = useState(0);
  const t = useT(strings);
  return (
    <main>
      <PageHead>{t.title}</PageHead>
      <PendingSection onChanged={() => setN((x) => x + 1)} />
      <CommandsSection reloadKey={n} />
    </main>
  );
}
