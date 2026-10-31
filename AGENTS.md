# AGENTS.md

Guidance for AI coding agents. The `README.md` is the canonical
architectural document; read it first. This file is the *how to change
things without breaking the design* companion.

---

## The load-bearing rules

If you take away nothing else from this file: **do not violate any of
these**, and if you think you have to, propose the change first
instead of implementing around it.

1. **The orchestrator must never parse plugin-owned data.** Instance
   configuration blobs, workflow step params, plugin-emitted outputs,
   and progress payloads are `json.RawMessage` / `[]byte` in Go and
   opaque values in JS. If you find yourself reaching for
   `json.Unmarshal` on any of them inside `orchestrator/`, stop -
   that's a bug in the design of what you're building. The plugin's
   own code and the plugin's own MFE are the only things allowed to
   understand those bytes.

2. **The plugin's MFE owns every plugin-facing UI surface.** Instance
   configuration forms, workflow-step configuration, action runtime
   panels, human-in-the-loop resolution prompts - all rendered by the
   plugin's React component, mounted by the shell with a `mode` prop.
   There is no JSON-Schema form renderer in the orchestrator and there
   will never be one. If you're tempted to add "just a small form" in
   the shell for a plugin's config, add an MFE slot instead.

3. **An instance is a stored config tuple, not a process.** One plugin
   binary serves N instances. A workflow step references an instance
   ID; on Execute the orchestrator hands the plugin both the step's
   opaque params and the instance's opaque config. Don't design
   features that require one plugin process per instance.

4. **Cross-step references are resolved by the plugin, not the
   orchestrator.** The orchestrator threads the bag of upstream
   outputs into `upstream_outputs`; the plugin's own Execute handler
   does substitution using whatever marker syntax it likes. Do not add
   `${step.X.output.Y}` templating, JSON-path resolution, or any
   schema-based wiring to the orchestrator.

5. **Durable execution lives in Temporal.** The orchestrator is
   stateless w.r.t. running workflows. Do not add in-memory run state,
   goroutines that outlive an HTTP request, or ad-hoc retry loops
   outside a Temporal Activity.

6. **Plugins that need a human suspend; they never block.** End the
   Execute stream with `ExecuteSuspend{prompt}` and declare the
   `resolution` slot. The orchestrator parks the step
   (`awaiting_resolution`), waits on a Temporal signal, and re-executes
   with `ExecuteRequest.resolution`. Never hold an Execute call open
   waiting for input, and never poll from the plugin. Keep the real
   answer in the plugin's own DB; the resolution blob should only steer
   (continue / abort).

7. **No stopgap workarounds.** If a change needs Temporal / real
   Postgres / a real secret store / a real message bus, wire it in.
   Don't build an interim in-memory / file-backed / mock substitute
   "until we have time to do it right" - those artifacts have a habit
   of outliving their intended lifetime and rotting the surrounding
   code.

8. **Only the orchestrator is browser-facing.** A plugin has one
   listener (gRPC) and is reachable only from the orchestrator. Its MFE
   bundle reaches the browser through `Plugin.GetAsset`, proxied at
   `/plugins/{id}/assets/…`, and all data goes through `/api/…`.
   Don't add an HTTP listener to a plugin, a manifest field holding a
   browser-facing URL, or a CDN URL in any bundle or in the shell. The
   shell serves React itself (`/vendor/`) and bundles PatternFly's CSS.
   The asset proxy streams bytes and never inspects them (rule 1).

9. **Plugins leave nothing on disk once a step is over.** Scratch
   space (clones, GOPATH) lives under the plugin's `--work-dir`, which
   it logs at startup. It is kept past the end of an Execute call only
   as a suspended step's resume cache. Every ending must remove it:
   - success, and *every* failure path, including ones before the
     plugin's own run record exists;
   - `ReleaseStep` from the orchestrator, which is the only way a
     plugin learns that a suspended step was cancelled;
   - a startup sweep, before serving, that deletes everything not
     provably live. This catches processes killed mid-run.

   Delete through `gitenv.RemoveAllWritable` (the Go module cache is
   read-only, so plain `os.RemoveAll` silently leaves it behind). Never
   let a "keep it for a while" TTL be the only cleanup for a state.

10. **Every command a plugin runs is recorded, in full.** Git/Go
    plugins execute through `pluginkit/runner`, with a
    `pluginkit/cmdlog.Recorder` attached
    (`cmdlog.NewRecorder(ctx, store.Commands, req).Attach(r)`, one
    recorder per Execute call, shared by every Runner it creates).
    Don't call `exec.Command` directly. If you truly must (a pipeline),
    record it by hand with `r.Record(kind, cwd, cmdLine)`, as
    `gitrebase.PatchIDs` does. The progress payload's log is a bounded
    live tail only. Never treat it as the record, and never grow it to
    hold everything: the history is served from the plugin's DB through
    `cmdlog_list` / `cmdlog_get`.

11. **No compatibility shims; change things in place.** The project
    is pre-release. Don't add deprecated functions, legacy fields,
    aliases, or Temporal `GetVersion` gates so that old callers, old
    data, or in-flight runs keep working. Change the code and update
    every caller in the same change. A change to the activities
    `ExecuteWorkflow` schedules breaks replay of open runs, so say in
    the PR that they must be cancelled or purged first. Generated code
    (`proto/gen/`) is exempt.

---

## Repository shape (at a glance)

```text
proto/                       shared gRPC contract module (Buf-generated Go stubs committed)
orchestrator/                Go service; PatternFly shell; Temporal client + worker
pluginkit/                   shared *plugin-side* Go + JSX: mfeassets (GetAsset, used by every
                             plugin) plus git-plugin mechanics (gitenv, refs, runner, godeps,
                             patchqueue, cmdlog, shellcmds, execstream; web/src shared components)
plugin-greeter/              minimal example plugin (gRPC-only + Vite MFE served via GetAsset)
plugin-generic-go-dep/       realistic example plugin: bumps a Go dep, runs the step's shell
                             commands, force-pushes to fork
                             (owns its own Postgres for the per-step patch queue)
plugin-upstream-rebase/      rebases a fork onto a new upstream tag (merge -s ours + re-pick
                             UPSTREAM commits), suspends for conflicts/decisions, then runs
                             the godeps pipeline and shell commands (owns its own Postgres on 5434)
workflows/                   *.yaml workflow files loaded on orchestrator startup; the only way to
                             define a workflow (a stored one whose file is gone is flagged removed)
docker-compose.yml           orchestrator Postgres + plugin-owned Postgres + Temporal + Temporal Web UI
temporal/                    Temporal dynamic config
```

Each plugin embeds its `web/dist/` output into its Go binary via
`//go:embed all:web/dist`. **Rebuild `web/dist` before rebuilding the
binary** if you touched any MFE source - otherwise you'll run the last
compiled bundle and the change will silently not appear. `web/dist` is
git-ignored, so a fresh checkout won't compile a plugin until its MFE
has been built once. The git plugins also bundle `pluginkit/web/src`
through a Vite `@pluginkit` alias: touching it means rebuilding *both*
`plugin-generic-go-dep/web` and `plugin-upstream-rebase/web`.

The orchestrator's `web/vite.config.js` also builds the `/vendor/*.js`
React entries that `index.html`'s import map points plugin bundles at.
The import map keys, the `VENDOR` table in that config, and the
externals list in every plugin's `vite.config.js` must stay in sync.
If a plugin needs another shared bare import, add it to all three.

---

## Standard workflow when making a change

1. **Read the affected code end-to-end** before proposing an
   implementation. This codebase is small enough that guessing is a
   waste of time; naming specific faults is faster than skimming.
2. **Rebuild the plugin(s) whose MFE you touched.**
   `cd plugin-<name>/web && npm install && npm run build` then restart
   the plugin binary. The orchestrator does not need a rebuild for
   plugin MFE changes.
3. **Rebuild the orchestrator's MFE** the same way if you touched
   `orchestrator/web/src`.
4. **If the schema changed:** bump `currentSchemaVersion` in
   `orchestrator/internal/storage/storage.go` and add a new
   `if have < N { ... }` block. Migrations are idempotent (`CREATE
   TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`). Never edit an
   already-shipped migration block.
5. **If the proto changed:** regenerate stubs (`cd proto && buf lint
   && buf generate`) and check the committed `proto/gen/` diff into
   the same PR. Non-additive changes are breaking; Buf's config is set
   to flag them at the file level.
6. **Update `README.md` and this file in the same change.** See the
   "Documentation upkeep" section below.

---

## Adding features: the decision tree

- **Does it need to render UI?** → Add or use an MFE slot on the
  plugin's side, mounted by the orchestrator's `PluginPanel`. Do not
  add plugin-domain UI to the shell.
- **Does it need per-plugin state?** → Live it on the plugin's own
  storage backend, keyed by the identity tuple in `ExecuteRequest`
  (`workflow_id`, `workflow_run_id`, `step_id`, `instance_id`,
  `attempt`, `idempotency_key`). Make `PurgeWorkflow` delete it.
- **Does it need cross-plugin state?** → Almost certainly a workflow
  step that produces an output the next step consumes. Don't add a
  cross-plugin store.
- **Does it need to run reliably in the background?** → Model it as a
  Temporal Workflow or Activity. Do not add "run in a goroutine after
  responding to the request" logic to the HTTP layer.
- **Does it need a form?** → Add a slot to the plugin and render the
  form in the MFE. If the form is orchestrator-owned (a workflow-level
  setting, not a plugin-domain setting), it goes in the shell.
- **Does it need a human mid-step?** → Suspend (rule 6) and render the
  prompt in the plugin's `resolution` slot.
- **Is it git / Go-module mechanics both git plugins need?** → It goes
  in `pluginkit/`, not copied between plugins. `pluginkit` is
  plugin-side code only - the orchestrator must never import it. Keep
  generic-go-dep's observable behaviour (commit messages, env, outputs)
  unchanged when refactoring shared code.

---

## Adding a plugin

1. New Go module with a single gRPC listener in `main.go`.
2. Implement `Plugin.Describe`, `Plugin.Execute` (server-streaming),
   `Plugin.Validate`, and `Plugin.GetAsset`.
   - `Describe` returns a manifest with a unique `id`, the `mfe_entry`
     path of the MFE module inside `web/dist` (`assets.Entry()`), one
     or more `actions` (each with named outputs), and the `mfe_slots`
     your MFE handles.
   - `Execute` emits zero or more `ExecuteEvent{Progress}` events
     followed by exactly one `ExecuteEvent{Final}` carrying `result` +
     `outputs`.
   - `Validate` switches on `kind`, returns `{ valid, errors[] }`.
   - `GetAsset` delegates to a `pluginkit/mfeassets.Server` built
     from the embedded `web/dist` (see any existing plugin's
     `main.go` / `grpc.go`).
   - `ReleaseStep` (optional) drops per-`(workflow_run_id, step_id)`
     working state once the orchestrator says the step is terminal.
     Required in practice if the plugin suspends (rule 9). Leave it
     unimplemented if nothing outlives an Execute call.
   - `PurgeWorkflow` (optional) deletes everything the plugin keeps for
     a `workflow_id` (durable per-step memory, per-run records, command
     history, leftover working copies) when the user purges a removed
     workflow. It must be idempotent, and it is called on every plugin.
     Implement it whenever the plugin stores anything keyed by
     `workflow_id` or by the workflow's runs.
3. Vite library bundle in `web/` with a React component that switches
   on `props.mode` and renders the right slot. Bundles must
   `external`-ise `react`, `react/jsx-runtime`, `react-dom`, and
   `react-dom/client` (see existing plugins' `vite.config.js`) so the
   shell's import map resolves them to the shell's `/vendor/` React,
   a single shared instance. Everything else is bundled or imported by
   relative URL; never from a CDN.
4. Embed `web/dist` into the Go binary with `//go:embed all:web/dist`.
5. Add the plugin's gRPC endpoint to the orchestrator's `--plugins`
   flag.

---

## Documentation upkeep - non-negotiable

**Every code change that alters observable behaviour must update
`README.md` and (where relevant) this file in the same commit.** This
is a load-bearing convention; the docs are the design document, not a
retrofit.

Trigger conditions:
- New / removed / renamed MFE slot → update the slots table in the
  README and any references in this file.
- Proto changes (fields added/removed, RPCs changed) → update the
  contract section in the README and the sample messages/snippets.
- New / changed HTTP endpoint → update the HTTP API reference in the
  README.
- New / renamed table or column → update the storage-model section in
  the README.
- New Temporal Activity / Workflow, or changed retry policy → update
  the execution-model section.
- New CLI flag or env var → update the configuration tables in the
  README.
- New load-bearing rule you'd want a future agent to follow → add it
  here.

If you touched code but genuinely nothing user-observable changed
(pure refactor, dead-code deletion, test fix), say so explicitly in
the PR description so the reader knows the doc omission is
intentional.

---

## Testing changes locally

Automated tests exist for `plugin-upstream-rebase` and for MFE asset
delivery:

```bash
# GetAsset (chunking, ETag, path escapes) and the orchestrator's
# /plugins/{id}/assets/ route (no services needed):
(cd pluginkit && go test ./mfeassets/)
(cd orchestrator && go test ./internal/httpapi/)
# Git mechanics against scratch repos (no services needed):
(cd pluginkit && go test ./shellcmds/)
(cd plugin-upstream-rebase && go test ./internal/gitrebase/)
# Full rebase action (suspend → decide → conflict → resolve → resume →
# push → reuse → next-release rebase) and the working-copy lifecycle
# (ReleaseStep, early-failure cleanup, startup sweep) against the
# plugin's real Postgres; skipped if it isn't reachable:
docker compose up -d rebase-postgres
(cd plugin-upstream-rebase && go test ./internal/service/)
```

Everything else is verified manually:

```bash
docker compose up -d
(cd orchestrator/web        && npm run build)
(cd plugin-greeter/web      && npm run build)
(cd plugin-generic-go-dep/web && npm run build)
(cd plugin-upstream-rebase/web && npm run build)
(cd plugin-greeter && go run . &)
# generic-go-dep / upstream-rebase need their own Postgres (5433 / 5434
# in docker-compose). Override with --pg-dsn / PG_DSN if hosted elsewhere.
(cd plugin-generic-go-dep && go run . &)
(cd plugin-upstream-rebase && go run . &)
(cd orchestrator && go run . --plugins=localhost:9000,localhost:9100,localhost:9200 --workflows=../workflows)
```

For the suspend / resolution path, run
`plugin-upstream-rebase/hack/make-fixture.sh` and follow what it
prints: it builds a local upstream/fork pair whose rebase stops for one
decision and one conflict.

Then open <http://localhost:8080>. For a real end-to-end exercise,
create a `github-acme-bot` instance under **Instances → Go Dependency
Updater** (see the README's "End-to-end" section) and run
`Example Rebase`. Watch the DAG update live via SSE
without polling - that's the smoke test for the wfengine + runbus +
SSE path.

If you can't test the UI, say so explicitly in the PR - don't claim
success on the basis of "compiles cleanly".

---

## Plugin-owned state - worked example

`plugin-generic-go-dep` demonstrates the "plugins own their own DB"
rule end-to-end. Its `internal/store` package speaks to a separate
Postgres (5433 in `docker-compose.yml`) and holds two tables:
`step_patches`, keyed by `(workflow_id, step_id, patch_id)`, and
`command_log` (see `pluginkit/cmdlog` below). The
`workflow-step-config` MFE reads and writes `step_patches` exclusively
through `pluginApi.execute(action, …, { instanceId, workflowId,
stepId })`, with `action` one of `patches_list`, `patches_upsert`,
`patches_delete` or `patches_reorder`. On
`Execute("update", …)`, `handleUpdate` fetches the same rows, groups
them by `target_dir`, and applies them after `go mod vendor` / before
`make` for each configured dir.

The patch queue itself (schema SQL, queries, `patches_*` handlers,
`ApplyPatch`, and the `PatchQueueEditor` JSX) lives in
`pluginkit/patchqueue` + `pluginkit/web`; `upstream-rebase` uses the
same queue against its own database.

`pluginkit/cmdlog` follows the same shape for the complete command
history (`command_log` table, `cmdlog_*` handlers, the
`CommandHistory` JSX). A plugin includes `cmdlog.TableSQL` in its own
migrations, exposes `store.Commands`, routes `cmdlog.Handle`, and
attaches a recorder to its runners (rule 10).

If you're adding another plugin that needs per-`(workflow, step)`
memory, copy this shape: separate Postgres in `docker-compose.yml`,
`--pg-dsn` + `PG_DSN` on the plugin binary, dedicated `internal/store`
package with its own `schema_version`, and MFE round-tripping through
new plugin actions rather than by reaching outside `pluginApi`.
`plugin-upstream-rebase/internal/store` is the larger worked example
(per-run tables keyed by `workflow_run_id`, reusable answers keyed by
`(workflow_id, step_id, …)`). Its step-runtime / resolution slots pass
`runId` to `pluginApi.execute` to read per-run state.

## When you're stuck

- Read the load-bearing rules again. Most confusion in this codebase
  comes from trying to bypass one of them.
- Prefer a smaller, uglier change that respects the rules over a
  cleaner change that violates them.
- If the design is genuinely wrong for what you need to do, say so and
  propose amending it. Don't paper over it with a workaround.
