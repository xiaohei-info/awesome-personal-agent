# apa-wake (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-wake`, then `hermes plugins enable apa-wake`. Gateway only.
Needs `apa` on PATH (or `APA_BIN`), `APA_URL`, and `APA_TOKEN` = the plugin's own worker/delegate token.

Opt-in (default stays headless): set `APA_WAKE_SESSION_KEY` to the main session to wake and allow injection with
`plugins.entries.apa-wake.allow_gateway_injection: true`. `readiness.py` is the one judgment of whether waking works
here: the plugin is enabled, the session key is set, injection is allowed, and `APA_URL`/`APA_TOKEN` are set. The
loop starts only when nothing is missing (otherwise only the `apa_wake_cli` tool is registered and the log names what
is missing), and `apa-headless` declares `main_session.wake: true` or the same missing items as `wake_missing` when the
gateway starts. After the owner confirms that declaration, they pick "continue in our chat" on the Agent card in Web
(or per rule); the card shows what is missing otherwise. Switching back and forth needs no new declaration.

Gateway only, by Hermes' own signal: Hermes loads the plugin in every process of the profile (gateway, dashboard,
`hermes chat`, `--oneshot` runs). The loop claims only while `readiness.in_gateway()` is true, i.e. Hermes'
`GatewayRunner` has published its plugin message injector (`PluginManager.has_gateway_message_injector`). Elsewhere,
and while the gateway is still starting or already stopping, it only checks that flag locally and never calls
`/deliveries/next`, so the gateway is the one claimer and the kernel's fail/backoff retries come back to it.

Flow per `class: wake` delivery: claim → `POST /runs/{id}/credential` → `ctx.inject_message(facts, session_key=…)` → ack
(`external_ref` = run id). Any failure → `/fail` (kernel backs off and retries); no chat-channel message is sent.
The run credential lives in memory only and reaches the turn via `apa_wake_cli` (`APA_RUN` + `APA_TOKEN`), re-exchanged
before expiry. Decide/accept/dismiss under it return exit 4 from the kernel.

Run-less deliveries on the same route (the kernel's proactive reminders, and brief/review/routine results the owner
chose to push into the main session; design §7.4) are notices: claim → `ctx.inject_message(facts)` → ack
(`external_ref` = `main-session`). No credential is exchanged, so nothing changes for the run boundary above. The text is
the kernel's title and lines plus one sentence on what to do next. A refused injection is failed back (kernel backoff,
then Web unread); a notice whose ack was lost is re-acked, not injected a second time, by the same gateway process.
