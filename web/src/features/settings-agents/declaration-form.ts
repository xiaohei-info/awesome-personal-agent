// 「接入其他 Agent」的表单数据 → 无头运行声明（附录 A.2）。只生成声明的一部分字段；其余字段用直接编辑。
// 内核 executor.register 仍是最终把关，这里的校验只为就地给出提示。

export const TIERS = ["read", "edit", "full"] as const;
export type Tier = (typeof TIERS)[number];

export interface OnboardForm {
  zh: string;
  en: string;
  /** 只在「显示原始值」里出现；空则由英文名生成。 */
  harness: string;
  command: string;
  tiers: Record<Tier, { on: boolean; args: string }>;
  resume: boolean;
  /** output：运行完在最后一行告诉 APA 会话编号；assign：启动时由 APA 指定。 */
  sessionFrom: "output" | "assign";
  output: string;
  assign: string;
  resumeArgs: string;
  wake: boolean;
}

export const emptyForm = (): OnboardForm => ({
  zh: "", en: "", harness: "", command: "",
  tiers: { read: { on: false, args: "" }, edit: { on: false, args: "" }, full: { on: false, args: "" } },
  resume: false, sessionFrom: "output", output: "", assign: "", resumeArgs: "", wake: false,
});

export interface Declaration {
  harness: string;
  name: { zh: string; en: string };
  headless: {
    command: string[];
    output?: string[];
    session: { extract?: string; assign?: string[] };
    resume?: string[];
    access: Partial<Record<Tier, string[]>>;
  };
  main_session?: { wake: true };
}

/** 按 shell 的规则拆参数：空白分隔，单双引号成组，不展开变量；引号没配对返回 null。 */
export function splitArgs(line: string): string[] | null {
  const out: string[] = [];
  let cur = "", quote = "", has = false;
  for (const ch of line) {
    if (quote) {
      if (ch === quote) quote = "";
      else cur += ch;
    } else if (ch === "'" || ch === '"') {
      quote = ch;
      has = true;
    } else if (/\s/.test(ch)) {
      if (has || cur) out.push(cur);
      cur = "";
      has = false;
    } else cur += ch;
  }
  if (quote) return null;
  if (has || cur) out.push(cur);
  return out;
}

/** Agent 标识：英文名转小写，非字母数字换成 -。 */
export const slug = (en: string) => en.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");

export const harnessOf = (f: OnboardForm) => f.harness.trim() || slug(f.en);

export type Field = "name" | "command" | "tiers" | "output" | "assign" | "resumeArgs" | "harness";
export interface Problem { field: Field; code: string; detail?: string }

const KNOWN = new Set(["{prompt}", "{run_dir}"]);

/** 表单里的问题，按字段给出；空表示可以提交。taken 是已有 Agent 的标识。 */
export function check(f: OnboardForm, taken: string[]): Problem[] {
  const ps: Problem[] = [];
  const add = (field: Field, code: string, detail?: string) => ps.push({ field, code, detail });
  if (!f.zh.trim() || !f.en.trim()) add("name", "name");
  const h = harnessOf(f);
  if (f.en.trim() && !h) add("name", "slug");
  else if (h && /\s/.test(h)) add("harness", "space");
  else if (h && taken.includes(h)) add("name", "taken");
  const cmd = splitArgs(f.command);
  if (!f.command.trim()) add("command", "command");
  else if (!cmd) add("command", "quote");
  else {
    const unknown = (f.command.match(/\{[^}]*\}/g) ?? []).find((p) => !KNOWN.has(p));
    if (unknown) add("command", "placeholder", unknown);
    else if (!cmd.some((a) => a.includes("{prompt}"))) add("command", "prompt");
  }
  if (!TIERS.some((t) => f.tiers[t].on)) add("tiers", "tiers");
  for (const t of TIERS) if (f.tiers[t].on && !splitArgs(f.tiers[t].args)) add("tiers", "quote");
  if (f.resume) {
    if (!splitArgs(f.output)) add("output", "quote");
    const r = splitArgs(f.resumeArgs);
    if (!r) add("resumeArgs", "quote");
    else if (!r.some((a) => a.includes("{session_id}"))) add("resumeArgs", "sessionId");
    if (f.sessionFrom === "assign") {
      const a = splitArgs(f.assign);
      if (!a) add("assign", "quote");
      else if (!a.some((x) => x.includes("{session_id}"))) add("assign", "sessionId");
    }
  }
  return ps;
}

const args = (s: string) => splitArgs(s) ?? [];

/** 生成声明。不续接时也要给内核一种会话取法：取 stdout 最后一行的 session_id，取不到不影响运行。 */
export function toDeclaration(f: OnboardForm): Declaration {
  const access: Partial<Record<Tier, string[]>> = {};
  for (const t of TIERS) if (f.tiers[t].on) access[t] = args(f.tiers[t].args);
  const headless: Declaration["headless"] = { command: args(f.command), session: { extract: "$.session_id" }, access };
  if (f.resume) {
    const out = args(f.output);
    if (out.length) headless.output = out;
    if (f.sessionFrom === "assign") headless.session = { assign: args(f.assign) };
    headless.resume = args(f.resumeArgs);
  }
  const d: Declaration = { harness: harnessOf(f), name: { zh: f.zh.trim(), en: f.en.trim() }, headless };
  if (f.wake) d.main_session = { wake: true };
  return d;
}

const shown = (a: string) => (a === "" || /[\s'"]/.test(a) ? `'${a.replace(/'/g, `'\\''`)}'` : a);

/** 内核拼参数的顺序（kernel/background、kernel/resume）：命令、输出、权限档，新开时再加指定会话，续接时加续接参数。
 * 占位原样显示，与输入框一致。 */
export function commandLine(d: Declaration, tier: Tier, resume = false): string {
  const h = d.headless;
  const tail = resume ? (h.resume ?? []) : (h.session.assign ?? []);
  return [...h.command, ...(h.output ?? []), ...(h.access[tier] ?? []), ...tail].map(shown).join(" ");
}
