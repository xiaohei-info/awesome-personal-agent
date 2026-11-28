import { describe, expect, it } from "vitest";

// 交互样式改版的守线（交互样式规格 §9 通用验收）：配色、字体一个不加，新变量只限尺寸、时长、阴影，动效时长走令牌。
// jsdom 不计算样式，这里直接读样式文件文本。
const css = import.meta.glob(["./**/*.css", "../features/**/*.css"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const file = (name: string) => css[name];
const strip = (s: string) => s.replace(/\/\*[\s\S]*?\*\//g, "");

const LIGHT = /:root\s*\{[\s\S]*?\n\}/;
const DARK = /:root\[data-theme="dark"\]\s*\{[\s\S]*?\n\}/;
const COLOR = /oklch\(|rgba?\(|hsla?\(|#[0-9a-fA-F]{3,8}\b|\b(?:black|white|red|green|blue|gray|grey|orange|yellow|purple)\b(?!-)/g;

/** 改版前两套主题的颜色变量（styles.css 主题块），名字与取值都不许动。 */
const THEME = {
  light: {
    paper: "oklch(96.8% 0.008 85)", surface: "oklch(99.2% 0.004 85)", sunken: "oklch(94.2% 0.01 85)",
    ink: "oklch(22% 0.015 60)", "ink-2": "oklch(42% 0.015 60)", "ink-3": "oklch(58% 0.014 60)",
    line: "oklch(89% 0.012 85)", "line-2": "oklch(80% 0.014 85)", seal: "oklch(54% 0.19 32)", "seal-soft": "oklch(94% 0.035 40)",
    ok: "oklch(48% 0.09 160)", "ok-soft": "oklch(94% 0.03 160)", warn: "oklch(52% 0.11 75)", "warn-soft": "oklch(94.5% 0.045 85)",
  },
  dark: {
    paper: "oklch(17% 0.008 60)", surface: "oklch(21% 0.009 60)", sunken: "oklch(14.5% 0.008 60)",
    ink: "oklch(93% 0.01 85)", "ink-2": "oklch(74% 0.012 85)", "ink-3": "oklch(57% 0.012 85)",
    line: "oklch(28% 0.01 60)", "line-2": "oklch(37% 0.012 60)", seal: "oklch(68% 0.17 35)", "seal-soft": "oklch(28% 0.06 35)",
    ok: "oklch(74% 0.1 160)", "ok-soft": "oklch(27% 0.04 160)", warn: "oklch(80% 0.11 82)", "warn-soft": "oklch(28% 0.045 80)",
  },
};

/** 一个块里的自定义属性。 */
function vars(block: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const m of block.matchAll(/--([\w-]+):\s*([^;]+);/g)) out[m[1]] = m[2].trim();
  return out;
}

/** 某类声明的值（不含主题块）：property 为正则源。 */
function decls(text: string, property: string): string[] {
  return [...strip(text).replace(LIGHT, "").replace(DARK, "").matchAll(new RegExp(`(?:^|[;{\\s])(${property})\\s*:\\s*([^;}]+)`, "g"))].map((m) => `${m[1]}: ${m[2].trim()}`);
}

describe("样式令牌守线", () => {
  const styles = file("./styles.css");
  const ui = file("./ui/ui.css");

  it("ui.css 里没有任何颜色字面量，只用现有变量与 color-mix()", () => {
    expect(strip(ui).match(COLOR) ?? []).toEqual([]);
  });

  it("styles.css 两个主题块之外的颜色字面量不多于改版前的 24 处", () => {
    const n = (strip(styles).replace(LIGHT, "").replace(DARK, "").match(COLOR) ?? []).length;
    expect(n).toBeLessThanOrEqual(24);
  });

  it("两个主题块里的颜色变量名字和取值与改版前完全一样；新增变量只有尺寸、时长、曲线、阴影与由现有颜色混出的状态底色", () => {
    const light = vars(strip(styles).match(LIGHT)![0]);
    const dark = vars(strip(styles).match(DARK)![0]);
    for (const [k, v] of Object.entries(THEME.light)) expect([k, light[k]]).toEqual([k, v]);
    for (const [k, v] of Object.entries(THEME.dark)) expect([k, dark[k]]).toEqual([k, v]);
    const known = new Set([...Object.keys(THEME.light), "sans", "serif", "mono"]);
    const allowed = /^(r(-\w+)?|row-h(-2)?|group-h|nav-row-h|bar-h|nav-w|sp-\d|dur-\w+|ease(-\w+)?|shadow(-\w+)?|hover|selected|cursor-ring)$/;
    const extra = [...Object.keys(light), ...Object.keys(dark)].filter((k) => !known.has(k) && !allowed.test(k));
    expect(extra).toEqual([]);
    // 状态底色只能由现有变量混出来
    for (const k of ["hover", "selected", "cursor-ring"]) expect(light[k]).toMatch(/^color-mix\(in oklch, var\(--ink(-3)?\) \d+%, transparent\)$/);
  });

  it("字体族只出现 var(--sans)、var(--serif)、var(--mono)", () => {
    for (const text of Object.values(css)) {
      for (const d of decls(text, "font-family")) expect(d).toMatch(/:\s*var\(--(sans|serif|mono)\)$/);
      for (const d of decls(text, "font")) expect(d).toMatch(/(:\s*inherit|var\(--(sans|serif|mono)\))(\s*!important)?$/);
    }
  });

  it("transition 与 animation 的时长都用 var(--dur-*)；循环的（骨架呼吸、转圈）例外", () => {
    const bad: string[] = [];
    for (const text of Object.values(css)) {
      for (const d of decls(text, "transition|animation|transition-duration|animation-duration")) {
        if (/\binfinite\b/.test(d)) continue;
        if (/(^|[\s,(])\d*\.?\d+m?s\b/.test(d.replace(/^[\w-]+:\s*/, ""))) bad.push(d);
      }
    }
    expect(bad).toEqual([]);
  });
});
