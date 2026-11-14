import type { ChangeEvent } from "react";
import { useLang, type Lang } from "./i18n";
import { useRaw } from "./raw";
import { CHANNEL_PREFIX, vocab, type Entry, type VocabKey } from "./vocab";

// 取值的显示（设计 §4.2）：界面只显示词表里的名字，ID 只在「显示原始值」打开时出现。查不到的 ID 原样显示。

/** 查一个取值的词条；渠道路由 channel:<名> 生成「推送到 <名>」。查不到时为 undefined。 */
export function entryOf(lang: Lang, k: VocabKey, id: string): Entry | undefined {
  const v = vocab[lang];
  if (k === "route" && id.startsWith(CHANNEL_PREFIX)) return { label: v.channelRoute(id.slice(CHANNEL_PREFIX.length)) };
  return (v[k] as Partial<Record<string, Entry>>)[id];
}

/** 取值的显示名（用于 aria-label、拼句等需要字符串的地方）；查不到时就是 ID 本身。 */
export function useLabel(): (k: VocabKey, id: string) => string {
  const lang = useLang();
  return (k, id) => entryOf(lang, k, id)?.label ?? id;
}

/** 一个取值：显示名；原始值打开或查不到时带上 ID。 */
export function Val({ k, id }: { k: VocabKey; id?: string | null }) {
  const lang = useLang();
  const raw = useRaw();
  if (!id) return null;
  const e = entryOf(lang, k, id);
  if (!e) return <code className="raw-id">{id}</code>;
  return (
    <>
      {e.label}
      {raw && <code className="raw-id">{id}</code>}
    </>
  );
}

/** 选一个取值的下拉：option 的 value 是 ID，文字是显示名。ids 缺省时列出词表里这一类的全部取值。 */
export function Pick({ k, value, onChange, ids, label, ...rest }: {
  k: VocabKey;
  value: string;
  onChange: (id: string) => void;
  ids?: readonly string[];
  /** 无障碍名称；外面已有 <label> 包着时可省。 */
  label?: string;
  disabled?: boolean;
  /** 放在最前的空选项的文字（如「全部」）；省略则没有空选项。 */
  empty?: string;
}) {
  const lang = useLang();
  const raw = useRaw();
  const list = ids ?? Object.keys(vocab[lang][k]);
  const text = (id: string) => {
    const name = entryOf(lang, k, id)?.label ?? id;
    return raw && name !== id ? `${name} · ${id}` : name;
  };
  return (
    <select aria-label={label} value={value} disabled={rest.disabled} onChange={(e: ChangeEvent<HTMLSelectElement>) => onChange(e.target.value)}>
      {rest.empty !== undefined && <option value="">{rest.empty}</option>}
      {list.map((id) => (
        <option key={id} value={id}>{text(id)}</option>
      ))}
    </select>
  );
}
