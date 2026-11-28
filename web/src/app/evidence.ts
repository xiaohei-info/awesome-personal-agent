// 回执证据（kernel/delegation.Evidence，附录 B.2）：links 是全部链接，link 是其中第一个（旧字段），较早的事件只有 link。
export interface Evidence { commit?: string; pr?: string; branch?: string; link?: string; links?: string[] }

/** 摊平成逐条的 [种类, 值]：每个链接一条。 */
export function evidenceEntries({ link, links, ...rest }: Evidence = {}): [string, string][] {
  const all: [string, string | undefined][] = [...Object.entries(rest), ...(links ?? [link]).map((l): [string, string | undefined] => ["link", l])];
  return all.filter((x): x is [string, string] => !!x[1]);
}
