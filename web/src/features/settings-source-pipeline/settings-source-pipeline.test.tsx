import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function server(sp: Record<string, unknown> = {}) {
  let version = 5;
  let doc: Record<string, unknown> = { version, source_pipeline: sp };
  let bands: { template: string; source?: string; low: number; high: number }[] = [];
  const puts: { url: string; body: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url.startsWith("/v1/agents")) {
      return json(200, {
        agents: [
          { id: "agent_main", harness: "hermes", name: "小黑", enabled: true, rev: 1 },
          { id: "agent_ops", harness: "hermes", name: "值班助手", enabled: true, rev: 1 },
        ],
        selected: { agent_id: "agent_main", agent_name: "小黑", selection_source: "main" },
        reason: "",
      });
    }
    if (url === "/v1/packs/inbound-triage/prompts/triage-screen") return json(200, { body: "内置判断标准" });
    if (url === "/v1/packs/inbound-triage/prompts/source-batch") return json(200, { body: "内置批处理说明" });
    if (url === "/v1/settings/source-pipeline" && method === "GET") return json(200, doc);
    if (url === "/v1/settings/source-pipeline" && method === "PUT") {
      const body = JSON.parse(init.body as string);
      puts.push({ url, body });
      doc = { version: ++version, source_pipeline: body.source_pipeline };
      return json(200, doc);
    }
    if (url === "/v1/settings/judgment" && method === "GET") return json(200, { version, bands, shadow: [], spot_check: { per_brief: 5 } });
    if (url === "/v1/settings/judgment" && method === "PUT") {
      const body = JSON.parse(init.body as string);
      puts.push({ url, body });
      bands = body.bands;
      return json(200, { version: ++version, bands });
    }
    if (url === "/v1/settings/policy") return json(404, { code: "not_found", why: "no policy in unit double" });
    return json(404, { code: "not_found", why: url });
  }) as unknown as typeof fetch;
  return { impl, puts };
}

function open(impl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/source-pipeline"]}>
      <App features={[feature]} fetchImpl={withSession(impl)} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("设置 › 来源处理", () => {
  it("按默认值渲染，不出现内核词或裸 id；改攒批条数、紧急开关和提示词按整段 PUT 保存", async () => {
    const s = server();
    open(s.impl);
    const main = await screen.findByRole("main");
    await screen.findByText("谁来处理");
    expect(main).not.toHaveTextContent(/source_pipeline|source_batch|triage-screen|agent_main/);

    const sched = screen.getByRole("region", { name: "多久处理一次" });
    const batchInput = within(sched).getByLabelText(/攒够几条就处理/);
    await userEvent.clear(batchInput);
    await userEvent.type(batchInput, "5");
    await userEvent.click(within(sched).getByLabelText("开"));
    await userEvent.click(within(sched).getByRole("button", { name: /^保存/ }));
    await waitFor(() =>
      expect(s.puts.at(-1)).toEqual({
        url: "/v1/settings/source-pipeline",
        body: { source_pipeline: { batch_size: 5, wait_minutes: 30, urgent_immediate: true, max_batch: 20 } },
      }),
    );

    const judgeCard = screen.getByRole("region", { name: "判断标准（哪些值得处理、哪些算紧急）" });
    await userEvent.type(within(judgeCard).getByRole("textbox"), "  报销和合同都算紧急  ");
    await userEvent.click(within(judgeCard).getByRole("button", { name: /^保存/ }));
    await waitFor(() =>
      expect(s.puts.at(-1)?.body.source_pipeline.judge_prompt).toBe("报销和合同都算紧急"),
    );

    await userEvent.click(within(judgeCard).getByText("查看内置默认"));
    expect(await within(judgeCard).findByText("内置判断标准")).toBeInTheDocument();
  });

  it("高级项：重试次数、每日上限与拿不准的范围各自校验与保存", async () => {
    const s = server({ batch_size: 10 });
    open(s.impl);
    await screen.findByText("高级");
    await userEvent.click(screen.getByText("高级"));

    const retryCard = screen.getByRole("region", { name: "重试与每日上限" });
    const retries = within(retryCard).getByLabelText(/失败后最多重试/);
    await userEvent.clear(retries);
    await userEvent.type(retries, "4");
    const daily = within(retryCard).getByLabelText(/每天最多处理/);
    await userEvent.clear(daily);
    await userEvent.type(daily, "15");
    await userEvent.click(within(retryCard).getByRole("button", { name: /^保存/ }));
    await waitFor(() =>
      expect(s.puts.at(-1)?.body.source_pipeline).toMatchObject({ max_retries: 4, daily_batch_limit: 15 }),
    );

    const bandCard = screen.getByRole("region", { name: "拿不准的范围" });
    const lo = within(bandCard).getByLabelText("下限");
    await userEvent.clear(lo);
    await userEvent.type(lo, "0.2");
    await userEvent.click(within(bandCard).getByRole("button", { name: /^保存/ }));
    await waitFor(() =>
      expect(s.puts.at(-1)).toEqual({
        url: "/v1/settings/judgment",
        body: { bands: [{ template: "triage-screen", low: 0.2, high: 0.6 }] },
      }),
    );
  });

  it("越界数字被前端直接挡下，不发请求", async () => {
    const s = server();
    open(s.impl);
    const sched = await screen.findByRole("region", { name: "多久处理一次" });
    const batchInput = within(sched).getByLabelText(/攒够几条就处理/);
    await userEvent.clear(batchInput);
    await userEvent.type(batchInput, "500");
    await userEvent.click(within(sched).getByRole("button", { name: /^保存/ }));
    expect(await within(sched).findByRole("alert")).toHaveTextContent("1 到 100");
    expect(s.puts).toHaveLength(0);
  });
});
