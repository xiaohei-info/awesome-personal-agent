import { Link } from "react-router-dom";
import { useT } from "../../app/i18n";
import { PERMISSION_GROUP } from "../../app/ids";
import { useLabel } from "../../app/values";
import type { PermissionCardDoc } from "./Permission";
import { strings } from "./strings";

// 没设过权限卡的 Agent 用默认卡：与 kernel/policy.DefaultCard 一致（查看、整理事项、处理来源事件；工具只读）。
const DEFAULT_CARD: PermissionCardDoc = { groups: { view: {}, organize: {}, source_events: {} }, tool: "read" };

/** 一个 Agent 的权限卡：GET /settings/access-limits 的 agent_cards 按声明存；没有就是默认卡。 */
export const cardOf = (cards: Record<string, PermissionCardDoc> | undefined, id: string) => cards?.[id] ?? DEFAULT_CARD;

/** 权限总览（设计 §14.5）：每个 Agent 的权限卡一览，只看，点名字去它自己的页面改——权限卡只在那一页改。没有 Agent 时不显示。 */
export function PermissionOverview({ agents }: { agents: { id: string; name: string; card: PermissionCardDoc }[] }) {
  const t = useT(strings);
  const label = useLabel();
  if (!agents.length) return null;
  const cell = (until: string | undefined, on: boolean) =>
    !on ? t.denied : until ? (new Date(until) < new Date() ? t.denied : t.allowedUntil(new Date(until).toLocaleString())) : t.allowed;
  return (
    <section aria-label={t.overview} className="panel" id="permission-overview">
      <h2>{t.overview}</h2>
      <p className="sub">{t.overviewHint}</p>
      <table>
        <thead>
          <tr>
            <th />
            {agents.map((a) => (
              <th key={a.id} scope="col"><Link to={"/settings/agents/" + encodeURIComponent(a.id) + "#permissions"}>{a.name}</Link></th>
            ))}
          </tr>
        </thead>
        <tbody>
          {PERMISSION_GROUP.map((g) => (
            <tr key={g}>
              <th scope="row">{label("permissionGroup", g)}</th>
              {agents.map((a) => {
                const gr = a.card.groups[g];
                return <td key={a.id}>{cell(gr?.until, !!gr)}</td>;
              })}
            </tr>
          ))}
        </tbody>
      </table>
      <p className="sub">{t.ownerOnly}</p>
    </section>
  );
}
