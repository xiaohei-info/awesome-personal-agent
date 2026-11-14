import { describe, expect, it } from "vitest";

// 文字预算（设计 §2）：页面上不出现成段解释，主界面不出现机制词。扫描全部 strings.ts 与 vocab.ts 的中文一份：
// - 任何一条文案（函数按占位参数取返回值）超过 MAX 个字符：成段文字，应改成命名、控件或预览；
// - 主界面文案里出现机制词：只允许出现在键路径含 advanced、raw、diag 的文案里（高级、原始值、诊断区）。
// 这一版先只报告不失败（STRICT = false）；各页改完后改为 true，此后新增违规就会让 CI 失败。
// 改完的目录先列进 STRICT_DIRS：这些目录里的违规现在就让测试失败。
export const STRICT = false;
export const STRICT_DIRS = [
  "features/today", "features/briefs", "features/unread", "features/decisions", "features/decision-screening",
  "features/proposals", "features/items", "features/projects", "features/materials", "features/materials-hosted",
  "features/agent-changes", "features/delegation", "features/attached-messages", "features/action-confirm", "features/forms",
  "features/sources",
];
export const MAX = 40;
export const BANNED = ["账本", "投影", "墓碑", "注入", "级联", "Event", "blob", "actor", "schema", "scope", "harness", "web_only"];
/** 有意保留的长文字：键名（各页文案展开了共用文案，所以按键名，不按文件）→ 理由。 */
const EXEMPT: Record<string, string> = {
  loginSub: "登录页的产品介绍，不是功能说明（设计 §5.1）",
};
const ALLOWED_PATH = /\.(advanced|raw|diag)\w*(\.|$)/i;

export interface Violation {
  where: string;
  kind: "long" | "banned";
  text: string;
}

function collect(v: unknown, path: string, out: [string, string][]) {
  if (typeof v === "string") out.push([path, v]);
  else if (typeof v === "function") {
    try {
      collect((v as (...a: unknown[]) => unknown)(1, "x", "y", "z"), path + "()", out);
    } catch {
      /* 需要特定参数的函数跳过 */
    }
  } else if (Array.isArray(v)) v.forEach((x, i) => collect(x, `${path}.${i}`, out));
  else if (v && typeof v === "object") for (const [k, x] of Object.entries(v)) collect(x, `${path}.${k}`, out);
}

/** 一份文案（中文）的违规清单。 */
export function violations(file: string, name: string, zh: unknown): Violation[] {
  const strings: [string, string][] = [];
  collect(zh, name, strings);
  const out: Violation[] = [];
  for (const [path, text] of strings) {
    const where = `${file}:${path}`;
    if (EXEMPT[path.split(".").pop()!]) continue;
    if ([...text].length > MAX) out.push({ where, kind: "long", text });
    if (!ALLOWED_PATH.test(path)) {
      const hit = BANNED.find((w) => text.toLowerCase().includes(w.toLowerCase()));
      if (hit) out.push({ where, kind: "banned", text: `${hit}：${text}` });
    }
  }
  return out;
}

const dicts = import.meta.glob(["../**/strings.ts", "./vocab.ts"], { eager: true }) as Record<string, Record<string, unknown>>;

function scan(): Violation[] {
  const out: Violation[] = [];
  for (const [f, mod] of Object.entries(dicts)) {
    const file = f.replace(/^\.\.\//, "").replace(/^\.\//, "app/");
    for (const [name, v] of Object.entries(mod)) {
      const d = v as { zh?: unknown; en?: unknown };
      if (d && typeof d === "object" && "zh" in d && "en" in d) out.push(...violations(file, name, d.zh));
    }
  }
  return out;
}

describe("文字预算", () => {
  it("检查本身：抓到长文字与机制词，放行高级区与豁免", () => {
    const v = violations("x/strings.ts", "strings", {
      ok: "保存",
      long: "这是一段很长的说明文字，用来解释某个设置在什么时候需要修改、修改以后会发生什么、默认值是多少。",
      word: "写进账本",
      advanced: { note: "写进账本" },
      fn: (n: number) => `已推送 ${n} 条，超出的会留在未读里，第二天零点重新计数，健康类和需要亲自确认的决策不计入`,
    });
    expect(v.map((x) => `${x.kind} ${x.where}`)).toEqual([
      "long x/strings.ts:strings.long",
      "banned x/strings.ts:strings.word",
      "long x/strings.ts:strings.fn()",
    ]);
  });

  it(STRICT ? "全部文案在预算内" : "报告当前违规（暂不失败）", () => {
    const found = scan();
    const byFile = new Map<string, number>();
    for (const v of found) {
      const f = v.where.split(":")[0];
      byFile.set(f, (byFile.get(f) ?? 0) + 1);
    }
    if (found.length) {
      console.info(
        `文字预算：${found.length} 处（长文字 ${found.filter((v) => v.kind === "long").length}，机制词 ${found.filter((v) => v.kind === "banned").length}）\n` +
          [...byFile].sort((a, b) => b[1] - a[1]).map(([f, n]) => `  ${n}\t${f}`).join("\n") +
          "\n" + found.map((v) => `  [${v.kind}] ${v.where}  ${v.text.slice(0, 60)}`).join("\n"),
      );
    }
    if (STRICT) expect(found).toEqual([]);
    else expect(found.filter((v) => STRICT_DIRS.some((d) => v.where.startsWith(d + "/")))).toEqual([]);
    expect(Object.keys(dicts).length).toBeGreaterThanOrEqual(25); // 至少扫到全部页面的文案
  });
});
