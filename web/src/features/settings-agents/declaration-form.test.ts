import { describe, expect, it } from "vitest";
import { check, commandLine, emptyForm, slug, splitArgs, toDeclaration, type OnboardForm } from "./declaration-form";

const form = (p: Partial<OnboardForm> = {}): OnboardForm => ({
  ...emptyForm(), zh: "甲", en: "Acme CLI", command: "acme run {prompt}", ...p,
  tiers: { ...emptyForm().tiers, edit: { on: true, args: "--write" }, ...p.tiers },
});

describe("接入其他 Agent 的表单 → 声明", () => {
  it("按 shell 规则拆参数：引号成组、空串保留，没配对返回 null", () => {
    expect(splitArgs(`a  "b c" 'd"e' ""`)).toEqual(["a", "b c", 'd"e', ""]);
    expect(splitArgs("a 'b")).toBeNull();
    expect(splitArgs("   ")).toEqual([]);
  });

  it("标识由英文名生成", () => {
    expect(slug(" Claude Code 2 ")).toBe("claude-code-2");
    expect(slug("测试")).toBe("");
  });

  it("不续接时给内核一种无害的会话取法；勾的档位参数可为空", () => {
    expect(toDeclaration(form({ tiers: { read: { on: true, args: "" } } as any }))).toEqual({
      harness: "acme-cli", name: { zh: "甲", en: "Acme CLI" },
      headless: { command: ["acme", "run", "{prompt}"], session: { extract: "$.session_id" }, access: { read: [], edit: ["--write"] } },
    });
  });

  it("启动时指定会话、能在对话里接着做", () => {
    const d = toDeclaration(form({ resume: true, sessionFrom: "assign", assign: "--sid {session_id}", resumeArgs: "--resume {session_id}", wake: true }));
    expect(d.headless.session).toEqual({ assign: ["--sid", "{session_id}"] });
    expect(d.main_session).toEqual({ wake: true });
    expect(commandLine(d, "edit")).toBe("acme run {prompt} --write --sid {session_id}");
    expect(commandLine(d, "edit", true)).toBe("acme run {prompt} --write --resume {session_id}");
  });

  it("每类问题各给一条", () => {
    const codes = (f: OnboardForm, taken: string[] = []) => check(f, taken).map((p) => p.code);
    expect(codes(form())).toEqual([]);
    expect(codes(form({ zh: "" }))).toEqual(["name"]);
    expect(codes(form({ en: "测试" }))).toEqual(["slug"]);
    expect(codes(form(), ["acme-cli"])).toEqual(["taken"]);
    expect(codes(form({ harness: "a b" }))).toEqual(["space"]);
    expect(codes(form({ command: "" }))).toEqual(["command"]);
    expect(codes(form({ command: "acme 'x {prompt}" }))).toEqual(["quote"]);
    expect(codes(form({ command: "acme {token} {prompt}" }))).toEqual(["placeholder"]);
    expect(codes(form({ command: "acme run" }))).toEqual(["prompt"]);
    expect(codes(form({ tiers: { edit: { on: false, args: "" } } as any }))).toEqual(["tiers"]);
    expect(codes(form({ resume: true, resumeArgs: "--resume" }))).toEqual(["sessionId"]);
    expect(codes(form({ resume: true, resumeArgs: "--resume {session_id}", sessionFrom: "assign", assign: "--sid" }))).toEqual(["sessionId"]);
  });
});
