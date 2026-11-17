import { Link } from "react-router-dom";
import { useT } from "../../app/i18n";
import { LinkWhere, isLink, type LinkInfo } from "../materials-hosted/LinkOpen";
import { strings } from "./strings";

// 材料与事项的类型化关系（§6.3）：边从事项指向材料；材料区按依据、产出、纪要分组（§12.8）。
export const MATERIAL_RELATIONS = ["basis", "output", "minutes"] as const;
export interface MaterialRow extends LinkInfo { id: string; title: string; kind?: string }
export interface MaterialRel { type: string; to: string; material: MaterialRow }

export function ItemMaterials({ rels, onRemove }: { rels: MaterialRel[]; onRemove: (r: MaterialRel) => void }) {
  const t = useT(strings);
  if (!rels.length) return null;
  return (
    <section aria-label={t.materials}>
      <h2>{t.materials}</h2>
      {MATERIAL_RELATIONS.map((type) => {
        const group = rels.filter((r) => r.type === type);
        if (!group.length) return null;
        return (
          <div key={type} role="group" aria-label={t.matGroups[type]}>
            <h3>{t.matGroups[type]}</h3>
            <ul className="rows">
              {group.map((r) => (
                <li key={r.to}>
                  <Link to={"/materials?id=" + encodeURIComponent(r.to)}>{r.material.title}</Link>
                  {isLink(r.material) && <> <LinkWhere m={r.material} /></>}{" "}
                  <button type="button" className="link" aria-label={t.removeRel(r.material.title, t.matGroups[type])} onClick={() => onRemove(r)}>{t.remove}</button>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </section>
  );
}
