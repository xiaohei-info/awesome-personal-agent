# apa-core (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-core`, then `hermes plugins enable apa-core`.
Needs `apa` on PATH (or `APA_BIN`) plus the usual `APA_URL` / `APA_TOKEN`.

`register(ctx)` calls `register(ctx)` of every submodule; later plugin nodes only add a submodule.
Sections are callables rendered once per new session (Hermes freezes the result); any apa failure
or a read over 2 s yields no section and never blocks the session.
