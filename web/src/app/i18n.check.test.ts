import ts from "typescript";
import { describe, expect, it } from "vitest";
import { browserLang } from "./i18n";

// 防漏翻（设计 §4.1 界面语言）：界面文案只能写在各 feature 的 strings.ts 里（或就地写成 { zh, en } 两份），
// 页面代码里出现中文字面量就失败；每份英文文案里不能出现中文。注释不受限制。
const HAN = /[\u4e00-\u9fff]/;
const sources = import.meta.glob("../**/*.{ts,tsx}", { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const dicts = import.meta.glob("../**/strings.ts", { eager: true }) as Record<string, Record<string, unknown>>;

const skip = (f: string) => /\.test\.tsx?$/.test(f) || f.endsWith("/strings.ts") || f.endsWith("/test-setup.ts");

/** 就地写的 { zh: "…", en: "…" }：zh 的值放行。 */
function inlineZh(node: ts.Node): boolean {
  const p = node.parent;
  if (!p || !ts.isPropertyAssignment(p) || p.initializer !== node || p.name.getText() !== "zh") return false;
  return ts.isObjectLiteralExpression(p.parent) && p.parent.properties.some((x) => x.name?.getText() === "en");
}

function hanLiterals(file: string, text: string): string[] {
  const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  const out: string[] = [];
  const visit = (n: ts.Node) => {
    const literal = ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n) || ts.isTemplateHead(n) || ts.isTemplateMiddle(n) || ts.isTemplateTail(n) || ts.isJsxText(n);
    if (literal && HAN.test(n.getText()) && !inlineZh(n)) {
      out.push(`${file}:${sf.getLineAndCharacterOfPosition(n.getStart()).line + 1} ${n.getText().trim().slice(0, 40)}`);
    }
    ts.forEachChild(n, visit);
  };
  visit(sf);
  return out;
}

/** 收集一份文案里所有字符串：函数用占位参数调用后取返回值。 */
function strings(v: unknown, path: string, out: [string, string][]) {
  if (typeof v === "string") out.push([path, v]);
  else if (typeof v === "function") {
    try {
      strings((v as (...a: unknown[]) => unknown)(1, "x", "y", "z"), path + "()", out);
    } catch {
      /* 需要特定参数的函数跳过 */
    }
  } else if (v && typeof v === "object") for (const [k, x] of Object.entries(v)) strings(x, `${path}.${k}`, out);
}

describe("界面文案的两种语言", () => {
  it("页面代码里没有中文字面量（文案登记在 strings.ts）", () => {
    const found = Object.entries(sources).filter(([f]) => !skip(f)).flatMap(([f, text]) => hanLiterals(f, text));
    expect(found).toEqual([]);
  });

  it("英文文案里没有中文（包括就地写的 { zh, en }）", () => {
    const bad: [string, string][] = [];
    for (const [file, mod] of Object.entries(dicts))
      for (const [name, v] of Object.entries(mod)) {
        const d = v as { en?: unknown };
        if (d && typeof d === "object" && "zh" in d && "en" in d) strings(d.en, `${file}:${name}.en`, bad);
      }
    for (const [file, text] of Object.entries(sources)) {
      if (skip(file)) continue;
      for (const m of text.matchAll(/en:\s*"([^"]*)"/g)) if (HAN.test(m[1])) bad.push([file, m[1]]);
    }
    expect(bad.filter(([, s]) => HAN.test(s))).toEqual([]);
  });

  it("缺省跟随浏览器：按偏好顺序取第一个 zh* 或 en*，都没有时用英文", () => {
    expect(browserLang(["zh-TW", "en"])).toBe("zh");
    expect(browserLang(["fr-FR", "en-GB", "zh"])).toBe("en");
    expect(browserLang(["fr-FR", "de"])).toBe("en");
    expect(browserLang([])).toBe("en");
  });
});

// 防止有人把检查本身改成只看部分目录：至少要扫到全部页面。
it("检查覆盖全部 feature", () => {
  expect(Object.keys(sources).filter((f) => f.includes("/features/") && f.endsWith("/index.tsx")).length).toBeGreaterThanOrEqual(25);
});
