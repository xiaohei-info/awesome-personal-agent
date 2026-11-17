import { locale, useT } from "../../app/i18n";
import { useNames } from "../../app/names";
import { Markdown } from "../items/Markdown";
import { strings } from "./strings";

// 所有显示简报的入口（简报页的最新与历史、/briefs/:id、事项详情）共用这一处：分节标题 + 按 Markdown 渲染的正文。
// 正文来自 spec.sections[].body 原文，不改写；渲染走 Markdown 组件（不走 innerHTML，只放行 http(s)/mailto 链接）。
export interface Section { project?: string; title?: string; body: string } // 附录 C.13：每节有 project 或 title 之一；旧简报只有 project
export interface Brief { id: string; title: string; created_at?: string; origin?: { source?: string }; spec?: { period?: string; sections?: Section[] } }

/** 一份简报的分节正文：节标题 h3，正文里的标题再下移，整份层级固定。 */
export function BriefBody({ spec }: { spec?: Brief["spec"] }) {
  const t = useT(strings);
  const projectName = useNames("project");
  const sections = spec?.sections ?? [];
  if (!sections.length) return <p className="sub">{t.noSections}</p>;
  return (
    <div className="brief-body">
      {sections.map((s, i) => {
        const title = s.title ?? projectName(s.project ?? "");
        return (
          <section key={i}>
            <h3>{title}</h3>
            <Markdown source={s.body ?? ""} from={4} dropTitle={title} />
          </section>
        );
      })}
    </div>
  );
}

export const when = (at?: string) => (at ? new Date(at).toLocaleString(locale()) : "");
