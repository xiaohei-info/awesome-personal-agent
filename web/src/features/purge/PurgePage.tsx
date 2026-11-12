import { useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 POST /v1/settings/purge（kernel/purge.Result）。授权（仅 Web 本人）、对象校验、墓碑与 blob 引用判断都在服务端；
// 本页只负责在不可逆操作前让本人确认，并如实展示服务端的回答。
interface Result {
  object: string;
  kind: string;
  events: number;
  blobs_removed?: string[];
  blobs_retained?: string[];
}

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

export function PurgePage() {
  const { api } = useSession();
  const [id, setId] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Result | null>(null);
  const object = id.trim();

  const purge = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await api.post<Result>("/settings/purge", { object }));
      setId("");
    } catch (e) {
      setError(msg(e));
    } finally {
      setConfirming(false);
      setBusy(false);
    }
  };

  return (
    <main>
      <PageHead lede="彻底删除一个事项或材料的内容：投影、正文和无其他引用的 blob 被删除，相关事件的内容替换为墓碑（序号、类型、时间、actor 保留）。">物理删除</PageHead>
      {error && <div role="alert">{error}</div>}
      {result && (
        <div role="status">
          已删除 {result.kind} {result.object}：{result.events} 条事件已墓碑化
          {result.blobs_removed?.length ? `；已删除 blob：${result.blobs_removed.join(", ")}` : ""}
          {result.blobs_retained?.length ? `；仍被其他材料引用而保留的 blob：${result.blobs_retained.join(", ")}` : ""}
        </div>
      )}
      <form
        className="panel"
        onSubmit={(e) => {
          e.preventDefault();
          if (object) setConfirming(true);
        }}
      >
        <label>
          对象 id
          <input value={id} onChange={(e) => setId(e.target.value)} disabled={confirming} />
        </label>
        <div>
          <button type="submit" disabled={!object || confirming}>删除…</button>
        </div>
      </form>
      {confirming && (
        <div role="alertdialog" aria-labelledby="purge-confirm">
          <p id="purge-confirm">
            将永久删除 <strong>{object}</strong> 的全部内容。此操作不可恢复，删除后无法找回。
          </p>
          <div className="actions">
            <button className="danger" onClick={purge} disabled={busy}>确认永久删除</button> <button onClick={() => setConfirming(false)} disabled={busy}>取消</button>
          </div>
        </div>
      )}
    </main>
  );
}
