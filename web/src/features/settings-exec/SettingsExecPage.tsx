import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";

// 形状对应 /v1/settings/{executors,declarations,commands}（kernel/executors）。授权（仅 Web 会话）、
// 声明校验、确认前不生效都在服务端；本页只负责调用并如实展示服务端的回答。
interface Executor {
  id: string;
  kind: string;
  host?: string;
  scenarios?: string[];
}
interface Declaration {
  harness: string;
  headless?: Record<string, unknown>;
  main_session?: Record<string, unknown>;
  executors?: Executor[];
}
interface Record_ {
  harness: string;
  rev: number;
  state: string;
  declaration: Declaration;
}

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

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
const DeclarationView = ({ d }: { d: Declaration }) => <pre data-testid={`declaration-${d.harness}`}>{JSON.stringify(d, null, 2)}</pre>;

function ExecutorsSection({ reloadKey }: { reloadKey: number }) {
  const { api } = useSession();
  const { items, error, setError, load } = useList<Executor>("/settings/executors", (r) => r.executors);
  const [id, setId] = useState("");
  const [kind, setKind] = useState("external-cli");
  const [host, setHost] = useState("");
  const [scenarios, setScenarios] = useState("");

  useEffect(() => {
    if (reloadKey) load();
  }, [reloadKey, load]);

  const add = async () => {
    setError(null);
    const body: Executor = { id: id.trim(), kind };
    if (host.trim()) body.host = host.trim();
    const sc = scenarios.split(",").map((s) => s.trim()).filter(Boolean);
    if (sc.length) body.scenarios = sc;
    try {
      await api.post("/settings/executors", body);
      setId("");
      setHost("");
      setScenarios("");
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };

  const remove = async (eid: string) => {
    setError(null);
    try {
      await api.del("/settings/executors/" + encodeURIComponent(eid));
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };

  return (
    <section role="region" aria-labelledby="h-executors">
      <h3 id="h-executors">执行者清单</h3>
      {error && <div role="alert">{error}</div>}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          add();
        }}
      >
        <label>
          执行者 id
          <input value={id} onChange={(e) => setId(e.target.value)} />
        </label>
        <label>
          种类
          <select value={kind} onChange={(e) => setKind(e.target.value)}>
            <option value="external-cli">external-cli</option>
            <option value="native-subagent">native-subagent</option>
            <option value="human">human</option>
          </select>
        </label>
        <label>
          主机
          <input value={host} onChange={(e) => setHost(e.target.value)} />
        </label>
        <label>
          场景（逗号分隔）
          <input value={scenarios} onChange={(e) => setScenarios(e.target.value)} />
        </label>
        <button type="submit" disabled={!id.trim()}>添加</button>
      </form>
      {items === null ? (
        !error && <p>加载中…</p>
      ) : items.length === 0 ? (
        <p>还没有执行者</p>
      ) : (
        <ul>
          {items.map((x) => (
            <li key={x.id}>
              <span>{x.id}</span> · {x.kind}
              {x.host && ` · ${x.host}`}
              {x.scenarios?.length ? ` · ${x.scenarios.join(", ")}` : ""} <button onClick={() => remove(x.id)}>删除</button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function PendingSection({ onChanged }: { onChanged: () => void }) {
  const { api } = useSession();
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
      <h3 id="h-pending">待确认声明</h3>
      <p>确认前声明不生效；确认意味着接受下列声明的全部内容，其中的命令将可在本机执行。</p>
      {error && <div role="alert">{error}</div>}
      {items === null ? (
        !error && <p>加载中…</p>
      ) : items.length === 0 ? (
        <p>没有待确认的声明</p>
      ) : (
        <ul>
          {items.map((r) => (
            <li key={`${r.harness}/${r.rev}`}>
              <span>{r.harness}</span> · rev {r.rev}
              <DeclarationView d={r.declaration} />
              <button onClick={() => decide(r, "confirm")}>确认</button> <button onClick={() => decide(r, "reject")}>拒绝</button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function CommandsSection({ reloadKey }: { reloadKey: number }) {
  const { api } = useSession();
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
      setError("声明不是合法的 JSON");
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
      <h3 id="h-commands">可执行命令</h3>
      {error && <div role="alert">{error}</div>}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          register();
        }}
      >
        <label>
          声明（JSON）
          <textarea rows={8} value={text} onChange={(e) => setText(e.target.value)} />
        </label>
        <button type="submit" disabled={!text.trim()}>登记</button>
      </form>
      {items === null ? (
        !error && <p>加载中…</p>
      ) : items.length === 0 ? (
        <p>还没有可执行命令</p>
      ) : (
        <ul>
          {items.map((r) => (
            <li key={`${r.harness}/${r.rev}`}>
              <span>{r.harness}</span> · rev {r.rev}
              <DeclarationView d={r.declaration} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export function SettingsExecPage() {
  // 确认/拒绝声明会改变清单与命令；用计数通知另外两个分区重新读取。
  const [n, setN] = useState(0);
  return (
    <main>
      <h2>执行者与声明</h2>
      <ExecutorsSection reloadKey={n} />
      <PendingSection onChanged={() => setN((x) => x + 1)} />
      <CommandsSection reloadKey={n} />
    </main>
  );
}
