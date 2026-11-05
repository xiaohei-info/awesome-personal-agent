# apa-wake (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-wake`, then `hermes plugins enable apa-wake`. Gateway only.
Needs `apa` on PATH (or `APA_BIN`), `APA_URL`, and `APA_TOKEN` = the plugin's own worker/delegate token.

Opt-in (default stays headless): set `APA_WAKE_SESSION_KEY` to the main session to wake and allow injection with
`plugins.entries.apa-wake.allow_gateway_injection: true`. Without the env var only the `apa_wake_cli` tool is registered.

Flow per `class: wake` delivery: claim → `POST /runs/{id}/credential` → `ctx.inject_message(facts, session_key=…)` → ack
(`external_ref` = run id). Any failure → `/fail` (kernel backs off and retries); no chat-channel message is sent.
The run credential lives in memory only and reaches the turn via `apa_wake_cli` (`APA_RUN` + `APA_TOKEN`), re-exchanged
before expiry. Decide/accept/dismiss under it return exit 4 from the kernel.
