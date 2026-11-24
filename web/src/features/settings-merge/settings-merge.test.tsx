import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const BUILTIN = "## 任务\n归类。\n## 四种结论\nattach merge duplicate keep\n## 判定依据\n同一件事。\n## 不许做的事\n不编造。";

// 替身按 /v1/settings/merge 的契约应答：PUT 按段替换，提示词不合格 400（内核 CheckMergePrompt）。
function server(merge: Record<string, unknown> = { exclude_sources: null, skip_existing: false, batch_size: 0, wait_minutes: 0, prompt: "" }, judge = "") {
  let doc: Record<string, any> = { version: 3, merge_judge: judge, merge };
  const puts: any[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_wechat", name: "微信" }, { id: "src_daxiang", name: "大象" }, { id: "src_mail", name: "gmail 邮件" }] });
    if (url === "/v1/packs/inbound-triage/prompts/merge-batch") return json(200, { body: "\n" + BUILTIN + "\n" });
    if (url === "/v1/merge/activity?days=7") return json(200, { days: 7, pending: 4, model_calls: 2, chars: 5300, batches: [{}, {}] });
    if (url === "/v1/settings/model-endpoints") return json(200, { generate_endpoint: { model: "gpt-6-luna" }, judge_endpoint: null });
    if (url === "/v1/settings/merge" && method === "GET") return json(200, doc);
    if (url === "/v1/settings/merge" && method === "PUT") {
      const body = JSON.parse(init.body as string);
      puts.push(body);
      doc = { ...doc, ...body, version: doc.version + 1 };
      if (body.merge?.prompt) doc.merge.prompt_at = "2030-01-01T00:00:00Z";
      return json(200, doc);
    }
    return json(404, { code: "not_found", why: url, next: "x" });
  }) as unknown as typeof fetch;
  return { impl, puts };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/merge"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const card = async (name: string) => within(await screen.findByRole("region", { name }));
const saveIn = async (name: string) => userEvent.click((await card(name)).getByRole("button", { name: `保存${name}` }));

beforeEach(() => sessionStorage.clear());

describe("设置 › 跨来源合并", () => {
  it("默认：与生成事项和建议相同、全部来源（含微信与大象）、纳入已有记录、100 条或 30 分钟、内置提示词；成本可见；界面不出现来源 id", async () => {
    mount(server().impl);
    expect((await card("由谁来合并")).getByLabelText(/与生成事项和建议相同（默认）/)).toBeChecked();
    const src = await card("哪些来源参与合并");
    await waitFor(() => expect(src.getByLabelText("微信")).toBeChecked());
    expect(src.getByLabelText("大象")).toBeChecked();
    expect((await card("和哪些已有记录比")).getByRole("checkbox")).toBeChecked();
    const batch = await card("批量参数");
    expect(batch.getByLabelText("攒够几条")).toHaveValue(100);
    expect(batch.getByLabelText("一批资料上限（token）")).toHaveValue(16000);
    expect(batch.getByRole("note")).toHaveTextContent("一批打包给模型的资料上限：超了就把这批砍半，余下的留到下一轮");
    expect(batch.getByRole("note")).toHaveTextContent("按约 1.5 个字符 ≈ 1 token 估算（不调用 tokenizer）");
    expect(batch.getByRole("note")).toHaveTextContent("默认 16000（约合原先的 24000 字），可填 2000 到 500000");
    await waitFor(() => expect(batch.getByRole("note")).toHaveTextContent("当前写作模型：gpt-6-luna")); // 与设置 › 模型同一份说明
    expect(batch.getByRole("note")).toHaveTextContent("超出会让那次调用失败，并按原有的失败处理重试");
    const p = await card("合并提示词");
    expect(p.getByRole("note")).toHaveTextContent("现在用的是内置默认");
    await waitFor(() => expect(p.getByLabelText("提示词正文")).toHaveValue(BUILTIN));
    expect(await screen.findByText(/跑了 2 批，模型调用 2 次，送出约 5300 字；还有 4 条等着成批/)).toBeInTheDocument();
    expect(screen.getByRole("main")).not.toHaveTextContent(/src_|merge_judge|inbound/);
  });

  it("取消勾选大象、不纳入已有记录：保存发整段 merge，其余字段原样带回", async () => {
    const s = server({ exclude_sources: null, skip_existing: false, batch_size: 10, wait_minutes: 0, prompt: "" });
    mount(s.impl);
    const src = await card("哪些来源参与合并");
    await userEvent.click(await src.findByLabelText("大象"));
    await saveIn("哪些来源参与合并");
    await waitFor(() => expect(s.puts.at(-1)).toEqual({ merge: { exclude_sources: ["src_daxiang"], skip_existing: false, batch_size: 10, wait_minutes: 0, prompt: "" } }));
    await userEvent.click((await card("和哪些已有记录比")).getByRole("checkbox"));
    await saveIn("和哪些已有记录比");
    await waitFor(() => expect(s.puts.at(-1).merge).toMatchObject({ exclude_sources: ["src_daxiang"], skip_existing: true, batch_size: 10 }));
  });

  it("提示词缺段：不发请求，原因写在卡片里；改对了才保存；一键恢复默认发空串", async () => {
    const s = server();
    mount(s.impl);
    const p = await card("合并提示词");
    const box = await p.findByLabelText("提示词正文");
    await waitFor(() => expect(box).toHaveValue(BUILTIN));
    await userEvent.clear(box);
    await userEvent.type(box, "只想留一句话 attach merge duplicate keep");
    await saveIn("合并提示词");
    expect(await p.findByRole("alert")).toHaveTextContent("少了：## 任务");
    expect(s.puts).toHaveLength(0);

    await userEvent.clear(box);
    await userEvent.type(box, BUILTIN.replace("同一件事。", "同一件事；周报不算。"));
    await saveIn("合并提示词");
    await waitFor(() => expect(s.puts.at(-1).merge.prompt).toContain("周报不算"));
    expect(await p.findByRole("note")).toHaveTextContent("现在用的是你改过的版本");

    await userEvent.click(p.getByRole("button", { name: "恢复默认" }));
    await waitFor(() => expect(s.puts.at(-1).merge.prompt).toBe(""));
    expect(await p.findByText("已恢复内置默认")).toBeInTheDocument();
    expect(box).toHaveValue(BUILTIN);
  });

  it("一批资料上限：改成 8000 随整段 merge 保存；改回默认不带这个键；0、负数、过小、过大、小数、空都不保存并写明范围", async () => {
    const s = server({ exclude_sources: ["src_mail"], skip_existing: false, batch_size: 10, wait_minutes: 0, prompt: "" });
    mount(s.impl);
    const batch = await card("批量参数");
    const box = batch.getByLabelText("一批资料上限（token）");
    for (const bad of ["0", "-1", "1999", "500001", "2500.5", ""]) {
      await userEvent.clear(box);
      if (bad) await userEvent.type(box, bad);
      await saveIn("批量参数");
      expect(await batch.findByRole("alert"), bad).toHaveTextContent("资料上限填 2000 到 500000 之间的整数（token）");
    }
    expect(s.puts).toHaveLength(0);
    await userEvent.clear(box);
    await userEvent.type(box, "8000");
    await saveIn("批量参数");
    await waitFor(() => expect(s.puts.at(-1)).toEqual({ merge: { exclude_sources: ["src_mail"], skip_existing: false, batch_size: 10, wait_minutes: 0, budget_tokens: 8000, prompt: "" } }));
    await waitFor(() => expect(box).toHaveValue(8000));
    await userEvent.clear(box);
    await userEvent.type(box, "16000");
    await saveIn("批量参数");
    await waitFor(() => expect(s.puts).toHaveLength(2));
    expect(s.puts.at(-1).merge).not.toHaveProperty("budget_tokens");
  });

  it("相似度档位：三档写明数值与一句说明，默认档选中且等于 0.30；改严随整段 merge 保存，改回默认不带这个键", async () => {
    const s = server({ exclude_sources: ["src_mail"], skip_existing: false, batch_size: 10, wait_minutes: 0, prompt: "" });
    mount(s.impl);
    const c = await card("相似度档位");
    expect(c.getByLabelText(/^默认（相似度 ≥ 0\.30）/)).toBeChecked();
    expect(c.getByLabelText(/^默认/).closest("label")).toHaveTextContent("和改成档位之前一样");
    expect(c.getByLabelText(/^宽（相似度 ≥ 0\.25）/).closest("label")).toHaveTextContent("漏合更少，模型调用和送出的字更多");
    expect(c.getByLabelText(/^严（相似度 ≥ 0\.40）/).closest("label")).toHaveTextContent("说法不同的同一件事可能漏合");
    await userEvent.click(c.getByLabelText(/^严/));
    await saveIn("相似度档位");
    await waitFor(() => expect(s.puts.at(-1)).toEqual({ merge: { exclude_sources: ["src_mail"], skip_existing: false, batch_size: 10, wait_minutes: 0, prompt: "", similarity: "strict" } }));
    await waitFor(() => expect(c.getByLabelText(/^严/)).toBeChecked()); // 以保存后的策略为准，不用重载
    await userEvent.click(c.getByLabelText(/^默认/));
    await saveIn("相似度档位");
    await waitFor(() => expect(s.puts).toHaveLength(2));
    expect(s.puts.at(-1).merge).not.toHaveProperty("similarity");
  });

  it("关闭合并：只发 merge_judge", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.click((await card("由谁来合并")).getByLabelText(/关闭合并/));
    await saveIn("由谁来合并");
    await waitFor(() => expect(s.puts.at(-1)).toEqual({ merge_judge: "off" }));
  });
});
