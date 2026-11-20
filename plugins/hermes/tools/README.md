# apa-tools (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-tools`, then `hermes plugins enable apa-tools`.
Needs `apa` on PATH (or `APA_BIN`) plus `APA_URL` / `APA_TOKEN`, Hermes v0.23.0 or later, and in the profile's `config.yaml`:

```yaml
terminal:
  client_env_passthrough: [APA_RUN_CONTEXT, APA_RUN]
```

Without both names `apa_declare_headless` refuses to declare. If runs start anyway (the line removed later, a session created before it, `apa declare` run by hand), the session's `APA_RUN` (set by the kernel on every run) or the run's PATH marker gives them away: file tools, CLI tools, hooks and a bare `apa` in `terminal` all error instead of using the gateway's credential. A new run is needed after adding the line, because Hermes freezes the client environment with the session.

A session without the marker cannot be told from an ordinary one and keeps the gateway's credential, by design: that is the owner's chat. The check reads the profile the tool call runs in, and `apa declare` run by hand bypasses it.

## Run identity

Tools and hooks run in Hermes' shared gateway process, so its cwd and environment say nothing about the run (a gateway started by a run's client even keeps that run's `APA_RUN` / `APA_TOKEN`). `run_context.py` is the single source: the session's bound `APA_RUN_CONTEXT` (Hermes freezes it from the client's environment and binds it for that session's turns) names the file the kernel wrote at launch with the run's id, credential, `run_dir` and `work_dir` (design §7.7). Every `apa` call in these plugins gets that path and never the inherited run variables; a session with no run uses the gateway's own credential. A session whose client env has `APA_RUN`, or whose client PATH has the kernel's marker (`.apa-run`), but no context is a run that lost it, and gets an error. The marker alone is not enough: a launcher script that sets PATH instead of extending it drops it.

Registers toolset `apa`: `apa_propose`, `apa_ask`, `apa_decide`, `apa_accept`, `apa_dismiss`, `apa_executors`, `apa_delegate`, `apa_report`,
`apa_attach`, `apa_proposals_apply`, `apa_item_progress`, `apa_dispatch`, `apa_item_history`, and the read-tier set `apa_read_file`, `apa_list_dir`, `apa_item_update`, `apa_material_commit`. Every CLI tool runs `apa` with `APA_SESSION=<Hermes session_id>`,
so the ledger audits to the session. The result is `{exit_code, ok, stdout, stderr}` with the kernel's verdict untouched
(exit 20 hard line, 21 executor not allowed, 23 tier closed, ... are the model's cue to stop; nothing is retried or faked).

## Delegation (design §6.17)

Hermes decides how to delegate, with its own tools. There is no "executor id -> invocation" table and nothing to configure.
`dispatch.py` holds Hermes' own shapes:

- **Executor ids are derived, not named by the model.** `apa_delegate` takes `mech` and its parameters and sends
  `apa delegate --assignee <id> --executor -` with the id, label, kind, host and executor handle below. Same way of delegating,
  same id; `harness` travels as `APA_HARNESS=hermes` and `host` is this machine unless paseo names another. A person is given
  with `assignee` and no `mech`.

  | mech | id | kind | label | executor handle |
  |---|---|---|---|---|
  | `delegate_task` | `hermes.delegate_task.<this host>` | native-subagent | `Hermes 子 agent @ <host>` | `{mech, host}` |
  | `paseo` + `profile` | `hermes.paseo.<host>.<profile>` | remote-agent | `Paseo · <profile> @ <host>` | `{mech, host, target?, profile}` |
  | `paseo` + `provider` [+ `model`] | `hermes.paseo.<host>.<provider>[.<model>]` | remote-agent | `Paseo · <provider>/<model> @ <host>` | `{mech, host, target?, provider, model?}` |

  Segments are lower-cased with anything outside `[a-z0-9_-]` turned into `-`; `<host>` is the machine part of paseo's
  `--host` (`user@mbp.local:22` -> `mbp`), kept verbatim as `target`. Ids over 64 characters keep a 55-character prefix plus
  8 hex of their SHA-1.
- **Dispatches are captured after the call** (apa-receipt's `post_tool_call`), when the arguments name a child item this
  Hermes session delegated (or read with `apa_item_history`):

  | Tool call | Dispatch record |
  |---|---|
  | `delegate_task` | no session, not resumable, handle `{mech: delegate_task, host}` |
  | `terminal`: `paseo run ...` or `paseo-profile-run [--host H] <profile> <prompt>` | session = the agent id printed under `AGENT ID` (or `agentId` in JSON), resumable when found, handle `{mech: paseo, host, target?, agent_id}`, `cwd` from `--cwd` |
  | `terminal`: `paseo send [--host H] <agent> ...` | same agent as session, `continues` = this process's last dispatch for that item and agent; when paseo says the agent is not found, that dispatch is marked `lost` instead |

  The record is written before the report, with an idempotency key; the kernel keeps one running record per item, harness
  and session, so a manual `apa_dispatch` for the same dispatch adds nothing. The "delegated in this session" set lives in
  the Hermes process and is lost on restart; `apa_item_history` refills it, and `apa_dispatch` covers the rest.
- **Handles are data.** `apa_dispatch` refuses handles outside the shapes above before calling `apa`. `apa_item_history`
  returns `apa context --scope item` with a `hermes` hint per dispatch: for records Hermes wrote whose handles match these
  shapes, how to resume with `terminal` (`paseo send ...`) or start anew the same way; everything else is
  `reference only`. The hint is advice; the model still decides and Hermes' own permissions apply.

No tool takes the owner's words, a grant or a session kind: the kernel decides from the key's scope and the tier the owner opened for this Agent in Web, nothing the caller reports (design §14.3).

## Live gate

`APA_REQUIRE_LIVE=1 HERMES_SRC=<hermes checkout> python3 -m pytest -q plugins/hermes/tests/tools/test_tools_live.py`

## Read tier (`-t apa,web`)

Hermes has no read-only file toolset, and task runs with `access: read` (brief, review, guideline-draft) must still read
`{materials_path}` and write their result back. The `apa` toolset covers that without `file` or `terminal`:

- `apa_read_file` / `apa_list_dir`: read-only, confined (symlinks resolved) to this run's `work_dir` (the run directory, or
  the project checkout for `cwd: project_checkout`) and `run_dir`; relative paths start at `work_dir`. Sibling runs and anything else are refused. With no run context (not a run, the config line above missing, the run ended) they refuse and say why, and never fall back to the process cwd. Files over 200 KB come back truncated and flagged. There is no write tool.
- `apa_item_update`: `apa item update <id> --spec <json> --reason ...`; the kernel's run identity still limits it to the run's own item.
- `apa_material_commit`: `apa material commit [<id>] --from - --reason ... [--kind --title --project]`; the text travels on stdin, never in a shell. It only creates a pending revision the owner confirms.

## Chat disposal tools

`apa_attach` (`apa attach <id> --to <item>`) and `apa_proposals_apply` (`--recommended` with optional `source`/`project`, or a list of `{id, do: accept|attach|dismiss, to?, reason?}` sent to `apa proposals apply --file -` on stdin, never a temp file) decide for the owner (tier 2): they only take effect if the owner opened the decide tier for this Agent, and each entry of a batch is judged on its own. `apa_item_progress` (`apa item progress <id> --text [--on]`) is tier 1 and always applies. On exit 23 the model must not retry: it asks the owner (`apa_ask`) or gives the Web link.
