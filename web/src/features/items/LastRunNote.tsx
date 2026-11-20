import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useT } from "../../app/i18n";
import { useFaultSay, type Fault } from "../../app/notice";
import { useSession } from "../../app/session";
import { strings } from "./strings";

// 这件事最近一次 Agent 运行没跑起来时的一句话（运行故障是事件，不是问题，§6.4）：出了什么事、结果没落账、下一步。
// 判定（是否故障、什么类别）都在服务端的 Run.fault；成功、在途或没有类别码的不显示。
export function LastRunNote({ item }: { item: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const say = useFaultSay();
  const [fault, setFault] = useState<Fault | null>(null);
  useEffect(() => {
    let live = true;
    api.get<{ runs: { state: string; fault?: Fault }[] }>("/runs?category=task,background&limit=1&target=" + encodeURIComponent(item)).then(
      (r) => live && setFault(r.runs[0]?.fault ?? null),
      () => live && setFault(null),
    );
    return () => { live = false; };
  }, [api, item]);
  const f = say(fault);
  if (!f) return null;
  return (
    <p role="note" className="panel">
      <strong>{t.runFault(f.say)}</strong> {t.notRecorded} {f.next} <Link to="/runs">{t.seeRuns}</Link>
    </p>
  );
}
