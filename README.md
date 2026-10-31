# upstream-readiness

A modular rebase engine for downstream projects and forks that need to
stay aligned with their upstream.

Keeping a downstream aligned with its upstream is recurring work: a set
of steps, some depending on others, repeated every time upstream moves.
upstream-readiness consists of an engine that orchestrates that work,
and rebase logic carried out by a **plugin**, intended to be owned by an
individual repository's owners. The engine's job is to run plugins'
steps in the right order, reliably.

At its core, the engine provides:

- **Deterministic Workflows.** A workflow is a graph of steps declared in
  a YAML file. Each step names a plugin, the action to run, and the
  steps it depends on. Independent steps run in parallel. A step that
  fails stops everything that depends on it, while unrelated branches
  carry on.
- **Steps that build on each other.** A step can publish named
  outputs, and the engine passes them to every step that depends on
  it, so one step's result can drive the next.
- **People (or agents) in the loop.** A step can pause and ask a question.
  The run waits, with no time limit and nothing held open, until
  someone answers, and the step then continues with the answer.
  Input that doesn't come from deterministic automation is noted and
  easy to review independently from the rest of the output.
- **Durable runs.** Runs execute on Temporal: failed steps are retried,
  a run survives restarts of the engine, and runs can start on demand
  or on a cron schedule. Every run and step is kept as history.
- **Shared configuration.** Credentials and settings live in named
  **instances** of a plugin, set up once and referenced by any number
  of steps.
- **One web UI.** Live progress and history for every run, plus every
  screen a plugin needs (configuration forms, progress panels,
  questions for a person). Plugins supply their own UI based on their
  unique needs, and the engine creates a single interface from them.

The engine never interprets what a plugin does or the data it passes
around. A plugin is a separate service with its own UI and its own
storage, so a new kind of step is a new plugin, and the engine doesn't
change.

This repository holds the engine and a few plugins built on it:

| Directory | What it is |
|---|---|
| `orchestrator/` | The engine: runs workflows on Temporal and serves the web UI |
| `plugin-upstream-rebase/` | Rebases a fork onto a new upstream release |
| `plugin-generic-go-dep/` | Bumps Go dependencies in a repo and pushes the result to a fork |
| `plugin-greeter/` | Minimal example plugin |
| `pluginkit/` | Code shared by the git plugins |
| `proto/` | The gRPC contract every plugin implements |
| `workflows/` | Workflow definitions, e.g. `openshift-kubernetes-rebase.workflow.yaml` |

---

## Architectural commitments

The engine lives in `orchestrator/`, and the rest of this document
calls it the orchestrator. Keeping it ignorant of what plugins do takes
a few firm rules. Everything else in the system follows from them.

1. **The orchestrator must never parse plugin-owned data.** Instance
   configuration blobs, workflow step params, and per-step outputs are
   all opaque bytes from the orchestrator's perspective. The plugin's
   MFE writes the bytes; the orchestrator persists them; the plugin's
   gRPC handler reads them. The orchestrator only ever marshals
   transport envelopes.

2. **The plugin's MFE owns every plugin-facing UI surface.** Instance
   configuration forms, workflow step editors, action runtime panels,
   and human-in-the-loop resolution prompts are all rendered by the
   plugin's React component. The orchestrator selects which surface to
   render with a `mode` prop. There is no JSON-Schema form renderer in
   the orchestrator and there will never be one.

3. **An instance is a stored config tuple, not a process.** One plugin
   binary serves N instances. A workflow step references an instance
   ID; on Execute the orchestrator hands the plugin both the step's
   opaque params and the instance's opaque config. This is what
   eliminates per-step copy-paste of credentials, git identity, push
   policy, etc.

4. **Cross-step references are resolved by the plugin, not the
   orchestrator.** Each plugin action declares named outputs in its
   manifest. After a step finishes, the orchestrator stores its outputs
   keyed by `(step_id, output_name)` and threads them into every
   downstream step's Execute call as `upstream_outputs`. The plugin's
   step params blob may contain marker objects in whatever syntax the
   plugin chooses; the plugin's own Execute handler does the
   substitution. The orchestrator does no string templating, no
   `${steps.X.output.Y}`, no schema-based wiring.

5. **Durable execution lives in Temporal.** A workflow run is a
   Temporal Workflow function. Each step is a Temporal Activity that
   gRPC-calls the plugin. Cron schedules are Temporal Schedules. Run
   state, retries, and recovery are Temporal's responsibility. The
   orchestrator process is stateless w.r.t. running workflows; it can
   be restarted at any time.

---

## Mental model

The smallest set of nouns that describes the system.

| Term | Definition |
|---|---|
| **Plugin** | A separate Go service that implements the `Plugin` gRPC contract, including `GetAsset`, which serves its embedded React MFE bundle. A plugin has exactly one listener (gRPC) and is never reached by the browser; the orchestrator proxies its MFE assets. Identified by a stable `id` it returns in its manifest. The orchestrator knows about a plugin only because it was told to dial its gRPC endpoint on startup. |
| **Manifest** | The plugin's self-description. Contains `id`, `display_name`, `version`, `mfe_entry`, `mfe_element`, the list of `actions`, and the list of `mfe_slots` the plugin's MFE implements. Fetched once at registration time. |
| **Action** | A named operation the plugin offers (e.g. `update`, `greet`). Declares zero or more **outputs** (named, free-text description, opaque bytes at runtime). The orchestrator never parses an action's params or its output values - but it does thread the outputs forward to downstream steps. Execute is server-streaming: the plugin emits zero or more opaque progress payloads (rendered by the `step-runtime` MFE slot) before a single `Final` event carrying `result` + `outputs`. |
| **Instance** | A persisted configuration record for a plugin. Has an ID (user-chosen or UUID), a plugin ID, a display name, and a plugin-owned opaque `config_blob`. Lives in Postgres. Created by the plugin's `instance-config` MFE slot; stored verbatim. Multiple instances per plugin are normal. |
| **Workflow** | A DAG of steps with a stable ID, a display name, and an optional cron schedule. Defined only by the `*.yaml` files in the `--workflows` directory, which are upserted into Postgres on startup; there is no API to create or edit one. A stored workflow whose file is gone at startup is flagged **removed**: it can't be run, but its history (and the state plugins keep for it) stays until it is **purged** or the file comes back. |
| **Step** | One node in the workflow DAG. References `(plugin, instance, action)`, declares `depends_on` step IDs, and carries an opaque `params` blob the plugin owns the shape of. |
| **Run** | One execution of a workflow. Has a `run_id` the orchestrator generates and uses as the Temporal workflow ID. Records `status` (`running` \| `succeeded` \| `failed`), `started_at`, `completed_at`, and the per-step results. |
| **Outputs** | The named, opaque-bytes values a step's action returned. Stored in the run summary as `outputs[output_name] = raw JSON`. Threaded into downstream steps' Execute calls as `upstream_outputs[upstream_step_id][output_name]`. |
| **MFE slot** | A named position in the orchestrator shell where the plugin's React component gets mounted with a `mode` prop telling it which surface to render. |
| **Suspension** | A step whose plugin ended its Execute stream with `ExecuteSuspend` instead of `Final`: it needs outside (human) input. The step sits in `awaiting_resolution`, the shell mounts the plugin's `resolution` slot with the opaque prompt, and when the slot answers, the orchestrator re-executes the step with the opaque answer in `ExecuteRequest.resolution`. The wait is a Temporal Signal wait in the workflow - no timeout, survives restarts. |

---

## System components

```text
  ┌─────────┐   HTTP :8080 - the only browser-facing port
  │ browser │   shell, /vendor/* (React), /api/*,
  │         │   /plugins/{id}/assets/* (plugin MFE bundles)
  └────┬────┘
       │
       ▼
┌─────────────┐  gRPC :9000/:9100/:9200  ┌────────────────────┐
│             │ ───────────────────────► │                    │
│             │  Describe                │  plugin process(es)│
│ orchestrator│  Execute                 │  (one Go binary    │
│   :8080     │  Validate                │   per plugin team, │
│             │  GetAsset (MFE bundle)   │   gRPC only)       │
└──────┬──────┘                          └────────────────────┘
       │
       │ Postgres :5432
       │ (instances, workflow_defs, run_summaries)
       │
       │ Temporal :7233
       │ (workflow runs, schedules, history, retries)
       ▼
```

- The **browser** loads the orchestrator's shell at `:8080` and never
  talks to anything else. React and PatternFly are bundled into the
  shell, so nothing comes from a CDN either. Whenever a plugin-owned
  surface needs to render, the shell dynamic-imports
  `/plugins/{id}/assets/{mfe_entry}`, which the orchestrator serves by
  streaming the plugin's `GetAsset` RPC. It then mounts the component
  and passes `(mode, manifest, pluginApi, slotContext)` as props.
- The **orchestrator** holds open gRPC connections to every registered
  plugin and forwards Execute / Validate / GetAsset / ReleaseStep /
  PurgeWorkflow calls. It owns no domain semantics.
- **Postgres** stores orchestrator-owned data only: plugin instances,
  workflow definitions, and a summary of each run. It does **not**
  store plugin-internal state - plugins are expected to own their own
  databases.
- **Temporal** is authoritative for execution state. The orchestrator's
  in-process Temporal worker registers one Workflow (`ExecuteWorkflow`)
  and the Activities `RunStep`, `RecordRunStart`, `RecordStepStart`,
  `RecordStepAwaiting`, `RecordStepFinish`, `ReleaseStep`, and
  `PersistResult` on the
  `upstream-readiness-workflows` task queue.

---

## The plugin contract

`proto/pluginsdk/v1/plugin.proto`

```protobuf
service Plugin {
  rpc Describe(DescribeRequest) returns (PluginManifest);
  rpc Execute(ExecuteRequest) returns (stream ExecuteEvent);
  rpc Validate(ValidateRequest) returns (ValidateResponse);
  rpc GetAsset(GetAssetRequest) returns (stream AssetChunk);
  rpc ReleaseStep(ReleaseStepRequest) returns (ReleaseStepResponse);
  rpc PurgeWorkflow(PurgeWorkflowRequest) returns (PurgeWorkflowResponse);
}
```

### Describe

Returns the plugin's `PluginManifest`. Fetched once when the
orchestrator registers the plugin. Fields:

| Field | Purpose |
|---|---|
| `id` | Stable identifier, e.g. `"generic-go-dep"`. Used as the foreign key for instances and as the workflow step's `plugin:` value. |
| `display_name` | Shown in the orchestrator UI. |
| `version` | Semver of this plugin build. |
| `mfe_entry` | Path of the MFE's ES module entry point inside the plugin's asset root (what `GetAsset` serves), e.g. `greeter-mfe.js`. The orchestrator exposes it to the shell as `/plugins/{id}/assets/{mfe_entry}`. |
| `mfe_element` | Default named export to mount when no slot overrides it. Empty string means default export. |
| `actions[]` | Each action has `name`, `description`, and `outputs[]` (each output has `name` + free-text `description`). |
| `mfe_slots[]` | The slot names the plugin's MFE handles, each optionally overriding `mfe_element`. |

### Execute

```protobuf
message ExecuteRequest {
  string action = 1;
  bytes payload = 2;                               // opaque step params
  string instance_id = 3;
  bytes instance_config = 4;                       // opaque
  map<string, StepOutputs> upstream_outputs = 5;   // keyed by step_id
  string workflow_run_id = 6;                      // per-run
  string step_id = 7;
  uint32 attempt = 8;                              // 1-indexed
  string idempotency_key = 9;                      // stable across retries
  string workflow_id = 10;                         // per-definition (empty for ad-hoc)
  bytes resolution = 11;                           // opaque answer from the "resolution" slot (resumes only)
}

message ExecuteEvent {
  oneof event {
    ExecuteProgress progress = 1;                  // zero or more
    ExecuteFinal    final    = 2;                  // terminal: step done
    ExecuteSuspend  suspend  = 3;                  // terminal: step needs input
  }
}

message ExecuteProgress { bytes payload = 1; }     // opaque, rendered by "step-runtime" slot

message ExecuteFinal {
  bytes result = 1;                                // returned to ad-hoc callers
  StepOutputs outputs = 2;                         // forwarded to downstream steps
}

message ExecuteSuspend { bytes prompt = 1; }       // opaque, rendered by "resolution" slot
```

Execute is a **server-streaming** RPC. A plugin emits zero or more
`ExecuteProgress` events followed by exactly one terminal event -
`ExecuteFinal` or `ExecuteSuspend` - then closes the stream.

**Suspend / resume.** A plugin that needs a human decision never blocks
inside Execute; it ends the stream with `ExecuteSuspend{prompt}`. The
orchestrator parks the step in `awaiting_resolution`, stores the prompt,
and mounts the plugin's `resolution` slot. When the slot calls
`onResolve(blob)`, the orchestrator signals the Temporal workflow and
calls Execute again for the same step with `resolution = blob`. A step
may suspend any number of times. Both `prompt` and `resolution` are
opaque; plugins usually keep the real answer in their own database and
use the blob only as "continue" / "abort". Suspension is only meaningful
inside workflow runs - an ad-hoc execute that suspends gets HTTP 409.

**Non-retryable failures.** A plugin returning gRPC `FailedPrecondition`
or `InvalidArgument` marks the failure as deterministic: the `RunStep`
activity fails immediately instead of retrying. Any other error code is
retried per the activity retry policy. The orchestrator forwards each progress
payload verbatim over SSE to any browser subscribed to the run and
persists the most recent one on `run_steps.progress_blob` so a page
reload or a late subscriber bootstraps to the current state. The
plugin's own MFE mounted in the `"step-runtime"` slot decides how to
render these opaque bytes.

`payload` and `instance_config` are JSON bytes the plugin owns. The
identity tuple `(workflow_id, workflow_run_id, step_id, instance_id,
attempt, idempotency_key)` is what makes idempotency and plugin-side
state possible - keyed correctly, a plugin can recognize retries, resume
from checkpoints, and cache decisions per-workflow (e.g. canned fix
commits) or per-run.

`StepOutputs.values` is `map<string, bytes>`. Each output is an opaque
JSON-encoded value. The orchestrator stores them per step and indexes
them by `(step_id, output_name)` when handing them to a downstream
step.

### Validate

```protobuf
message ValidateRequest {
  string kind = 1;            // "instance" | "step"
  bytes payload = 2;
  string instance_id = 3;     // populated when kind == "step"
  bytes instance_config = 4;  // populated when kind == "step"
  map<string, StepOutputs> upstream_outputs = 5;
  string workflow_id = 6;     // populated when kind == "step" inside a workflow
}

message ValidateResponse {
  bool valid = 1;
  repeated string errors = 2; // human-readable, surfaced verbatim
}
```

Called on instance save and on workflow save so plugins can reject
malformed blobs without the orchestrator parsing them.

### GetAsset

```protobuf
message GetAssetRequest {
  string path = 1;           // relative to the asset root, e.g. "greeter-mfe.js"
  string if_none_match = 2;  // browser's If-None-Match, forwarded
}

message AssetChunk {
  oneof part {
    AssetHeader header = 1;  // first message, exactly once
    bytes data = 2;          // then zero or more chunks
  }
}

message AssetHeader {
  string content_type = 1;
  string etag = 2;           // quoted strong validator
  bool not_modified = 3;     // if_none_match matched; no data follows
}
```

Serves one file of the plugin's MFE bundle so the browser never needs
to reach the plugin. The orchestrator exposes it as
`GET /plugins/{id}/assets/{path}` and streams the bytes through
unchanged, along with the plugin's content type and ETag. Responses are
sent with `Cache-Control: no-cache`, so the browser always revalidates
with the ETag and a rebuilt plugin's bundle is picked up immediately.
Because everything lives under one path prefix, relative imports inside
a bundle (code-split chunks, `new URL("./x.svg", import.meta.url)`)
also resolve through the orchestrator. The data is chunked so bundles
of any size stay under gRPC's message limit. Plugins return `NotFound`
for unknown paths (HTTP 404) and `InvalidArgument` for paths that
escape the root (HTTP 400). Any other error becomes HTTP 502. All three
in-repo plugins implement this with `pluginkit/mfeassets`, which
serves the plugin's `//go:embed`'d `web/dist`.

### ReleaseStep

```protobuf
message ReleaseStepRequest {
  string workflow_run_id = 1;
  string step_id = 2;
  string workflow_id = 3;
  string instance_id = 4;
  string action = 5;
  string status = 6;   // "succeeded" | "failed" | "cancelled" - informational
}
message ReleaseStepResponse {}
```

The orchestrator calls this once a step that reached the plugin is
terminal and will never run or resume again. The plugin then drops
whatever it kept for that `(workflow_run_id, step_id)`. This is how a
plugin learns that a step it **suspended** was cancelled. Nothing else
tells it, because the cancel happens while no Execute call is open.
Durable per-`(workflow_id, step_id)` memory (saved resolutions, patch
queues) is not released.

The call is best-effort and must be idempotent. It is retried a bounded
number of times, it may arrive for a step that already cleaned up after
itself, and a failure never fails the run. A plugin with nothing to
release can leave it unimplemented: the orchestrator treats
`Unimplemented` as success. `greeter` and `generic-go-dep` do that.
`upstream-rebase` deletes the step's working copy and marks a run
record still `running` / `suspended` as `cancelled`.

Plugins still need a startup sweep for working state left by a process
that died mid-run. Both git plugins sweep their `--work-dir` before
serving.

### PurgeWorkflow

```protobuf
message PurgeWorkflowRequest  { string workflow_id = 1; }
message PurgeWorkflowResponse { map<string, int64> deleted = 1; } // per plugin-chosen category, shown verbatim
```

The orchestrator calls this when the user purges a **removed** workflow
(see *Workflow* in the mental model). The plugin deletes everything it
keeps for `workflow_id`:

- durable per-`(workflow_id, step_id)` memory (patch queues, saved
  resolutions, decisions, rerere cache);
- per-run records and command history of the workflow's runs;
- any working copies still on disk for them.

It is called only after none of the workflow's runs is still running,
and **every** registered plugin is asked, including ones that never held
anything for the workflow. So it must be idempotent: a purge that failed
part-way is simply retried. `Unimplemented` counts as "nothing to purge".
Any other error aborts the purge before the orchestrator deletes
anything of its own, so the workflow stays listed and the purge can be
retried. `greeter` leaves it unimplemented. `generic-go-dep` deletes its
patch queue and command history. `upstream-rebase` deletes working
copies first, then all of its tables for the workflow, in one
transaction.

---

## MFE slots

The orchestrator mounts the plugin's React component (loaded from
`/plugins/{id}/assets/{mfe_entry}`) in different positions in the
shell. Which position is indicated by the `mode` prop passed to the
component. A plugin declares
which slots it handles via `manifest.mfe_slots[]`; slots not declared
are simply not mounted (the orchestrator either renders its own
fallback or skips that surface).

| Slot | `mode` | When mounted | What `slotContext` looks like |
|---|---|---|---|
| Action runtime | `"action"` | The user navigated to **Plugins** → *plugin* and is running a one-shot action. Not mounted when the plugin also declares `plugin-home`. | none |
| Plugin home | `"plugin-home"` | Sidebar → **Plugins** → *plugin*, when the plugin declared this slot. Replaces the ad-hoc action panel with the plugin's own landing surface. | none |
| Instance config | `"instance-config"` | The user is creating or editing an instance via **Instances** → *plugin* → New instance / Edit. | `{ existing: Instance \| null, onSaved: (Instance) => void }` |
| Instance list item | `"instance-list-item"` | Inline row content in the Instances list - lets the plugin surface a summary line without the orchestrator parsing `config_blob`. | `{ instance: Instance }` |
| Sidebar badge | `"sidebar-badge"` | Small badge next to the plugin's sidebar entries (Plugins and Instances). Useful for live counts / status indicators. | `{ kind: "plugin" \| "instances" }` |
| Step runtime | `"step-runtime"` | Under the workflow DAG, one card per step whose status is `running`; also inside the run details' per-step **Details** expander for steps that already finished, so plugin-rendered results (e.g. the rebase commit table) stay visible. Receives the latest progress payload the plugin's Execute stream emitted. | `{ runId, workflowId, stepId, instanceId, action, status, progress: <opaque bytes or null> }` |
| Workflow step config | `"workflow-step-config"` | Modal that opens when the user clicks a step node in the DAG. Nodes whose plugin declares this slot carry a pencil badge on their top-right corner. Per-`(workflow_id, step_id)` memory the plugin owns - `generic-go-dep` and `upstream-rebase` use this slot for their patch queue (see *Per-step patch queue* below); `upstream-rebase` also shows its saved conflict resolutions / decisions. | `{ workflowId, stepId, instanceId, action, params }` |
| Step editor | `"step-editor"` | The user is editing a workflow step's params blob. *(Contract present; orchestrator UI for it not yet built.)* | `{ existing: Step \| null, upstreamOutputs: [{step_id, output_name, description}], onSaved: (Step) => void }` *(planned)* |
| Resolution | `"resolution"` | A **Needs input** card under the DAG, one per step in `awaiting_resolution` (the plugin suspended it). The plugin renders its own prompt and calls `onResolve(blob)`; the orchestrator re-executes the step with that opaque blob. | `{ runId, workflowId, stepId, instanceId, action, status, progress, prompt: <opaque>, onResolve: (blob) => Promise }` |

Every MFE component receives:

```jsx
function MyPanel({ manifest, pluginApi, mode, slotContext }) { ... }
```

### `pluginApi`

A small object the orchestrator hands to every MFE mount. The plugin
must round-trip *only* through this surface - never directly through
`fetch` or `window.location`.

```js
pluginApi.execute(action, payload, { instanceId, workflowId, stepId, runId })
  // POST /api/plugins/:pluginId/execute
  // ad-hoc invocation; requires instanceId
  // workflowId + stepId + runId are optional scoping the orchestrator
  // forwards to the plugin (as workflow_id / step_id / workflow_run_id)
  // so it can key its own per-(workflow, step) and per-run state
  // (used by the workflow-step-config, step-runtime, and resolution slots)
  // returns the parsed JSON result blob

pluginApi.listInstances()
  // GET /api/instances?plugin=:pluginId
  // returns Array<Instance>

pluginApi.saveInstance({ id, display_name, config_blob })
  // POST /api/instances (create) or PUT /api/instances/:id (update)
  // config_blob is whatever the plugin wants; opaque to the orchestrator
  // returns the persisted Instance
```

When the plugin's MFE produces a slot result (a saved instance, an
edited step, a resolution decision), it calls back through `pluginApi`
or via `slotContext`'s callbacks; the orchestrator persists the blob
exactly as received.

---

## Cross-step output threading

This is the mechanism by which a downstream step uses an upstream
step's result. The orchestrator's only job is to thread the bag of
opaque outputs through; the **plugin** decides how to encode references
in its params blob and how to resolve them on Execute.

The example `generic-go-dep` plugin uses a `__ref` marker object:

```yaml
target_version:
  __ref: { step: update-lib-core, output: commit }
```

To the orchestrator, that's just a JSON object inside the params blob.
On Execute, the orchestrator hands the plugin:

- `payload` = the entire params blob (with the `__ref` still in it)
- `upstream_outputs["update-lib-core"]["commit"]` = the raw JSON bytes
  the upstream step emitted as its `commit` output

The plugin walks the blob looking for its own marker, substitutes the
upstream value in, then proceeds. A different plugin could use a
different marker syntax (`${step.id.output}`, `<<ref:...>>`, whatever);
the orchestrator wouldn't notice.

This is the design payoff: chaining works without any orchestrator code
needing to understand what a "commit" or a "branch" is.

### Auto-replace for fork-based chains (`generic-go-dep`)

`generic-go-dep` extends the __ref idea with one plugin-domain
convenience. When a step bumps a module to a `target_version` that came
from an upstream step's `commit` output *and* that same upstream step
also emitted a `fork_url` output, the plugin infers that the referenced
commit only exists on the fork (because the upstream PR hasn't merged
yet). It then adds a Go `replace` directive to the cloned repo's
`go.mod`:

```text
replace github.com/your-org/lib-core => github.com/acme-bot/lib-core <pseudo-version>
```

`go mod edit -replace` rejects raw commit SHAs on the RHS, so the plugin
first resolves the fork SHA to a canonical pseudo-version via
`go mod download -json` and writes that. The plugin also installs
`insteadOf` rewrites through `GIT_CONFIG_COUNT`+`GIT_CONFIG_KEY_n`
overrides so `https://<host>/...` URLs the Go module tooling generates
resolve over SSH using the instance's key - critical when the fork is
private and the Go module proxy has never seen it.

The replace lives in the committed `go.mod`; reviewers see it
explicitly in the PR and know to drop it once the upstream module
absorbs the fork commit.

Because updates are a list, this happens **per bump**. A fan-in step
that bumps two modules (`lib-utils` and `lib-auth`), each pinned to a
different upstream step's commit, ends up with two independent replace
directives - one per module - in the same commit. Multi-module repos
extend the same idea across dirs: each entry in `dirs[]` has its own
`updates` list, so a monorepo with independent go.mod files can be
handled with one clone / one branch / one push.

The `update` action declares five outputs downstream steps (and
external consumers) can bind to:

| Output | Value |
|---|---|
| `ref` | Branch name pushed to the fork. |
| `commit` | Full SHA of the tip commit pushed (the last dir's last commit on the branch). Used by downstream `target_version` refs to pin to the exact commit. |
| `repo` | The upstream `clone_url` the change was based on. |
| `fork_url` | The fork the branch was pushed to. Downstream auto-replace pairs this with `commit`. |
| `bumps` | List of `{path, dependency, target_version, replace?}` records - one per applied update, in the order dirs were processed. `path` is the module dir the bump landed in (`"."` for the repo root). Downstream tooling (a future PR-opening plugin, a changelog generator) can render these without parsing commit messages. |

### Per-step patch queue (`generic-go-dep`, `upstream-rebase`)

Both git plugins share this feature through `pluginkit/patchqueue`
(storage SQL + queries, the `patches_*` action handlers, `ApplyPatch`)
and `pluginkit/web/src/PatchQueueEditor.jsx`; each plugin keeps the
table in its own database. `upstream-rebase` additionally tags every
commit a queued patch produces with a `Rebase-Patch-Id: <patch_id>`
trailer - see *upstream-rebase* below.

Some upstream bumps need a canned local fix on top - a build-tag
tweak, a `go.mod` edit, a shim for a rename the mechanical bump can't
know about. `generic-go-dep` lets a workflow author attach an ordered
list of git patches to any `(workflow_id, step_id)` and re-apply them
on every run of that step.

Where they live:

- **Storage:** `plugin-generic-go-dep/internal/store` speaks to a
  plugin-private Postgres (the `godep-postgres` service in
  `docker-compose.yml`, exposed on port 5433). One table,
  `step_patches`, keyed by `(workflow_id, step_id, patch_id)`, with an
  ordinal column controlling apply order. The orchestrator has no
  handle to this schema (load-bearing rule 3 - plugins own their own
  databases).
- **UI:** the `workflow-step-config` MFE slot. Click a step node in
  the workflow DAG and the plugin's panel renders the attached patch
  list plus an inline editor for name, target dir (picked from the
  step's configured `dirs`), and patch body (paste or upload a
  `.patch` / `.diff` file). Reorder with up/down, delete inline.
  `patches_upsert` without an `ordinal` appends a new patch and leaves
  an edited one where it is. The
  MFE round-trips through `pluginApi.execute` using four new plugin
  actions: `patches_list`, `patches_upsert`, `patches_delete`, and
  `patches_reorder` - all scoped by the `workflow_id` + `step_id`
  that the shell forwards on the execute call.
- **Application:** during Execute, for each configured dir, the
  plugin fetches patches whose `target_dir` matches and applies them
  in ordinal order **after** `go get` / `go mod tidy` / `go mod
  vendor` and **before** the dir's make targets. Patches that begin
  with `From <sha> …` (mbox from `git format-patch`) go through
  `git am --3way`, preserving the author and message baked into the
  patch. Everything else is treated as a plain unified diff and
  applied with `git apply --3way`, then committed with a plugin-
  generated message that names the patch. The patch text is written
  to a scratch file inside the clone's `.git` dir, never the worktree,
  so the commit contains only what the patch changes. A patch that produces no
  staged changes fails the step loudly - silently dropping a queued
  patch is exactly the kind of stale-config surprise the queue exists
  to prevent.
- **Ad-hoc / no workflow:** patch fetching is skipped when the
  Execute call has no `workflow_id` (invocations from the plugin's
  ad-hoc `action` MFE), so testing a bump in the plugin's own runtime
  UI never picks up a per-workflow queue that wasn't asked for.

### Shell commands (`generic-go-dep`, `upstream-rebase`)

Both git plugins accept an optional `commands` list in their step
params, so a workflow can run arbitrary repo scripts (code generators,
`hack/update-*.sh`, …) as part of the step:

```yaml
commands:
  - name: regenerate openapi           # required; single line, becomes the commit subject
    run: "hack/update-openapi-spec.sh" # required; handed to `sh -c`
    dir: "."                           # optional; relative to the repo root, default "."
```

Shared code is in `pluginkit/shellcmds`. The commands run in order,
after everything else the step commits (every dir's bumps / vendor /
patch queue / make targets) and before the push. Each `run` goes
through `sh -c` in `dir` on the recorded runner, so it appears in the
command history like any other command. Whatever a command changes is
committed on its own, with `$ <run>` in the commit body. A command that
leaves the tree unchanged gets no commit. A non-zero exit fails the
step. `dir` follows the same rules as a `dirs[].path` (relative, no
`..`). Any value may be a `__ref` marker.

Commit subjects carry each plugin's usual prefix:

| Plugin | Subject |
|---|---|
| `generic-go-dep` | `chore: <name>` / `chore(<dir>): <name>` |
| `upstream-rebase` | `UPSTREAM: <drop>: <name>` / `UPSTREAM: <drop>: (<dir>) <name>` |

`commands` is edited in the workflow YAML only. The ad-hoc action
panels don't expose it.

### Command history (`generic-go-dep`, `upstream-rebase`)

Progress events carry only the last ~4KB of the command log
(`runner.TailMax`), which keeps SSE traffic small. The complete record
lives in each plugin's own Postgres, in a `command_log` table (shared
code in `pluginkit/cmdlog`). It holds one row for every entry the
step's `runner.Runner` produced:

| `kind` | What | Written |
|---|---|---|
| `command` | logged commands (`Run`, `ResolvePseudo`): clone, fetch, cherry-pick, `go mod tidy`, `make`, push, … | inserted when the command starts and completed when it ends, so a command that hangs or is killed still shows up, unfinished |
| `query` | read-only bookkeeping (`Output`, `ExitCode`): rev-parse, rev-list, `git show :1:path`, the patch-id pipeline, … | once, when done |
| `note` | `Runner.Note` lines (`# resuming from checkpoint …`) | once |

Each row has the working dir, start and finish times, exit code (`-1`
when the process didn't exit normally), error text, and the **full**
output. Output is stored as `bytea` because git output may contain NUL
bytes or invalid UTF-8. It's converted to valid UTF-8 when served.

Rows are keyed by `(workflow_run_id, step_id)` and grouped by
`execution_id`. Every Execute call is a new execution: the first run,
each resume after a suspend, and each Temporal retry, tagged with its
`attempt`. History writes use a context detached from the Execute call
(10s timeout each), so a cancelled run still records how its last
command ended. A failed write never fails the run; the plugin logs the
first failure per execution. Nothing deletes history: `ReleaseStep`
and the work-dir sweep drop working copies, not rows. There is no
retention limit, so the table grows by roughly the total output of
every run.

**Actions** (MFE-backing, called with `runId` + `stepId` scoping):

- `cmdlog_list {after_id?, limit?, kinds?, q?}` returns
  `{entries, more, counts}`, without output bodies. Pages are up to
  2000 rows (default 500), in id order. `q` matches the command line
  (case-insensitive) or the output (exact bytes). `counts` is per kind
  for the whole step, ignoring filters.
- `cmdlog_get {id}` returns `{entry}` with the full output. It only
  returns entries inside the requested `(run, step)`.

**UI** (`pluginkit/web/src/CommandHistory.jsx`):

- The list is grouped by execution. Each row shows status, command,
  duration, and output size. Clicking a row loads its full output.
- Filters: commands / queries / notes (queries hidden by default),
  failed only, and server-side search over commands and output.
- "Load more" pages through long histories.
- While the step runs, each progress event pulls new and still-running
  entries (at most every 1.5s).

Where it's mounted:

- `generic-go-dep`'s `step-runtime` slot. Ad-hoc runs from the action
  panel go to run/step `ad-hoc`, shown on the plugin's home page under
  **Ad-hoc run history**.
- `upstream-rebase`'s `step-runtime` slot, as the **Command history**
  expander.

Both work for finished runs through the run details' **Details**
expander.

### upstream-rebase

`plugin-upstream-rebase` maintains a fork of an upstream project whose
fork-only commits follow the `UPSTREAM: <type>: message` convention:

| Subject | Meaning | What the rebase does |
|---|---|---|
| `UPSTREAM: <carry>: …` | carried forward forever | cherry-picked |
| `UPSTREAM: <drop>: …` | never re-applied (typically generated content) | skipped (`dropped`) |
| `UPSTREAM: 12345: …` | upstream PR #12345, carried until upstream merges it | cherry-picked; if the pick is empty, it's `already_upstream` |

Bracketed / unbracketed variants (`carry`, `<12345>`) are accepted.
Anything else starting with `UPSTREAM` is **malformed**, and a commit
with no prefix is **unclassified**. Both need a human decision; the
plugin never guesses.

**What one `rebase` step does** (`internal/service/rebase.go`,
`internal/gitrebase/`):

1. Clone `clone_url` at `fork_branch`, and fetch every upstream tag into
   `refs/upstream/tags/*`, plus `target_ref`.
2. **Find the previous rebase.** Walk the fork branch's first-parent
   chain for the newest merge whose tree equals its first parent's tree
   (the signature of `merge -s ours`) and whose first parent is an
   upstream tag commit. `previous_rebase_commit` overrides this, but is
   verified the same way. Refuses a target that isn't newer than the
   previous base.
3. **Enumerate everything that must be considered:**

   ```sh
   git log --reverse --topo-order --no-merges <fork> ^<prev-merge> ^<target> --not --glob=refs/upstream/tags/*
   ```

   Excluding upstream-reachable commits drops anything that arrived via
   mid-cycle upstream merges. Merges in the same range whose
   `git diff-tree --cc` is non-empty (**evil merges**, content that
   lives only in a merge) are listed too, because cherry-picking can't
   carry them. Every row is written to the plugin DB before anything is
   picked.
4. **Gate.** Unclassified, malformed, and evil-merge rows need a saved
   decision. If any are missing, the step **suspends once** and asks for
   all of them together.
5. `git checkout -b rebase/<target> <target>`, then
   `git merge -s ours <fork tip>`.
6. **Pick in order.** Each outcome:
   - **clean** → `picked`
   - **empty PR** → `already_upstream`
   - **empty carry** → suspends to ask whether to drop it or keep it as
     an empty commit
   - **conflict** → see *Conflicts* below
7. **Completeness check.** It fails the step if:
   - any row is still non-terminal,
   - any new commit isn't on the branch, or
   - the number of commits on top of the new merge differs from the
     number of rows that produced one.

   As a review aid, each re-applied commit is compared with its original
   by `git patch-id` (two batched passes). Only pairs whose change
   differs (conflict resolutions, absorbed context) get a one-to-one
   `git range-diff`, stored for review and flagged "differs from
   original" in the table. A whole-range range-diff pairs every old
   commit with every new one and doesn't finish on histories with large
   generated-file commits.
8. **Dependency phase.** For each `dirs` entry, run the exact
   generic-go-dep pipeline (`pluginkit/godeps`):

   ```text
   bumps → go mod tidy → vendor → the step's patch queue → make targets
   ```

   `updates` may be empty here (e.g. just `make update`).
9. **Shell commands.** Each `commands` entry (`{name, run, dir?}`) runs
   in order, after every dir's pipeline, so it comes after every other
   commit the plugin makes (`pluginkit/shellcmds`, see *Shell commands*
   above). `run` goes through `sh -c` in `dir`
   (relative to the repo root, default `"."`), and the recorded runner
   captures it like any other command. Any changes are committed on
   their own as `UPSTREAM: <drop>: [(dir) ]<name>`, with the command in
   the body. A command that leaves the tree unchanged gets no commit,
   and a non-zero exit fails the step. `commands` works without `dirs`.
10. `git push --force` to the fork and emit outputs:
    - `ref`, `commit`, `repo`, `fork_url`
    - `target_ref`, `target_commit`, `previous_rebase_commit`
    - `bumps`
    - `commits`: `[{orig_sha, new_sha, subject, kind, pr, status}]`

**Conflicts and reuse.** A conflict's fingerprint is a sha256 over the
unmerged index entries (path, stage, mode, blob), so identical base,
upstream, and fork content means an identical fingerprint. Resolution
order:

1. **Exact saved resolution** for `(workflow_id, step_id, fingerprint)`:
   always applied automatically, in any later run of the pipeline. The
   table marks it `human` (answered during this run) or `reused`.
2. **git rerere**, per the step's `rerere` param:
   - `off`: disabled.
   - `suggest` (default): rerere's result is pre-filled in the
     resolution panel for a human to confirm.
   - `auto`: if rerere resolves every path, it's committed without
     asking (`rerere` source).

   The rerere cache persists per `(workflow_id, step_id)` in the
   plugin DB.
3. Otherwise **suspend**. The resolution panel shows base, upstream
   ("ours") and fork-commit ("theirs") versions of every conflicted
   file. For each file you can edit the merged result (markers,
   suggestion or earlier resolutions as starting points), take a side,
   or delete it. You can also skip the whole commit with a reason.
   Answers with conflict markers left in are rejected.

**Decisions** are keyed by commit SHA. A `carry` decision also covers
later commits with the identical subject, since re-picked commits get
new SHAs every rebase. This fallback can only keep a commit, never drop
one: `drop` decisions must be re-confirmed per commit.

**Everything the plugin generates is `UPSTREAM: <drop>: …`** (dependency
bumps, vendor, make, queued patches, shell commands), so the next
rebase drops and regenerates it. Every commit produced by the **patch
queue** also gets a `Rebase-Patch-Id: <patch_id>` trailer. The next rebase lists it as
`requeued_patch` and re-applies it from the queue rather than picking
it, so an mbox patch carrying its own `UPSTREAM: <carry>:` message is
never applied twice.

**Resume.** While suspended, the working copy stays under `--work-dir`
(`<work-dir>/<run>/<step>`) with a checkpoint (HEAD) in the plugin DB:

- If the copy is present and matches the checkpoint, the resume
  continues in place.
- Otherwise it replays from scratch. Saved resolutions and decisions
  make the replay deterministic, so plugin restarts and Temporal
  retries are safe.

**Working-copy cleanup.** A copy is kept only while its step is
suspended. Every other ending removes it, along with the `<run>/` dir
once it's empty:

- **Success, or failure inside Execute**, including failures before
  the run row exists (e.g. a bad `previous_rebase_commit` after the
  clone): `finish` / `fail` remove it.
- **The orchestrator ends the step while it's suspended** (run
  cancelled): `ReleaseStep` removes it and marks the run `cancelled`.
- **The plugin was killed mid-run, or a `ReleaseStep` was missed**: at
  startup, before serving, the plugin deletes everything under
  `--work-dir` except copies of runs still `suspended` and updated in
  the last 7 days.

**UI** (`web/src/RebasePanel.jsx`):

- `step-runtime`: progress plus the **commit table**:
  - columns: #, original SHA, kind (PR links when upstream is on
    GitHub), subject/author, status, new SHA, notes
  - filters and a range-diff viewer
  - re-fetched via `run_get` when the progress payload's `table_rev`
    changes
  - a **Command history** expander covering every command of every
    execution, with full output (see *Command history* above)
- `resolution`: the decision form or the conflict resolver, plus
  **Abort**.
- `workflow-step-config`: the patch queue, and a **Memory** tab
  (described below).

**Memory and wiping it.** Each step's memory is its saved resolutions,
decisions, and rerere cache. The **Memory** tab supports:

- **Selective deletion**: pick any mix of resolutions, decisions, and
  rerere entries and delete them (`memory_delete`).
- **Scoped wipe**: wipe this step, or every rebase step of the workflow
  (`memory_wipe`). Categories are selectable: resolutions, decisions,
  rerere cache, patch queue, finished run history.

Rules for what gets deleted:

- Every rerere entry is linked to the resolution (or rerere
  auto-resolution) and conflicted path that produced it, read from
  git's `MERGE_RR` when the conflict is resolved.
- Deleting a resolution also deletes its rerere entries. An entry git's
  `MERGE_RR` didn't name when it was exported has no known origin and
  shows as "unlinked".
- Wiping run history spares running and suspended runs.
- When a suspended run resumes, it prunes its working copy's rerere
  cache to what's still stored, so deleted entries don't come back.

| Status | Meaning |
|---|---|
| `pending` | not processed yet |
| `picked` | cherry-picked cleanly |
| `resolved` | picked after a conflict resolution (`human`, `reused`, or `rerere`) |
| `already_upstream` | PR commit became empty: upstream has it |
| `dropped` | `<drop>`, or a drop decision |
| `requeued_patch` | produced by the patch queue last time; re-applied from the queue |
| `kept_empty` | became empty; kept by decision |
| `skipped` | skipped by a conflict resolution |
| `acknowledged` | evil merge acknowledged |
| `awaiting_decision` / `conflict` | waiting for a human (non-terminal) |

Try it locally with `plugin-upstream-rebase/hack/make-fixture.sh`. It
builds a small upstream/fork pair that exercises a decision, a
conflict, a drop, and an already-merged PR, and prints the workflow to
register.

---

## Storage model

`orchestrator/internal/storage/storage.go` owns these tables. Schema
migrations are inline and versioned via a `schema_version` table.

### `plugin_instances`

```sql
id           TEXT PRIMARY KEY
plugin_id    TEXT NOT NULL
display_name TEXT NOT NULL
config_blob  JSONB NOT NULL   -- opaque; plugin-owned shape
created_at   TIMESTAMPTZ
updated_at   TIMESTAMPTZ
```

### `workflow_defs`

```sql
id           TEXT PRIMARY KEY
display_name TEXT NOT NULL
schedule     TEXT NOT NULL DEFAULT ''   -- cron expression, "" = manual
definition   JSONB NOT NULL             -- full Definition (incl. opaque step params)
created_at   TIMESTAMPTZ
updated_at   TIMESTAMPTZ
removed_at   TIMESTAMPTZ                -- set when the YAML file was gone at startup (schema v5)
```

Startup upserts every file in `--workflows` (clearing `removed_at`) and,
only if the whole directory loaded cleanly, sets `removed_at` on every
stored workflow without a file. A removed workflow can't be run. Purging
it (`POST /api/workflows/:id/purge`) deletes its `run_steps`,
`run_summaries`, and `workflow_defs` rows in one transaction, but only
after every plugin's `PurgeWorkflow` and the Temporal cleanup succeeded.

### `run_summaries`

```sql
run_id       TEXT PRIMARY KEY
workflow_id  TEXT NOT NULL
status       TEXT NOT NULL   -- running | succeeded | failed
started_at   TIMESTAMPTZ NOT NULL
completed_at TIMESTAMPTZ
results      JSONB           -- the full RunResult JSON (steps[], outputs)
```

`run_summaries` is a denormalised mirror of what the Temporal workflow
emits at the end of a run. Temporal is authoritative; this table exists
so the UI can list and inspect runs without paging Temporal history on
every request.

### `run_steps`

```sql
run_id        TEXT NOT NULL
step_id       TEXT NOT NULL
plugin        TEXT NOT NULL
action        TEXT NOT NULL
status        TEXT NOT NULL   -- running | awaiting_resolution | succeeded | failed | skipped | cancelled
started_at    TIMESTAMPTZ NOT NULL
completed_at  TIMESTAMPTZ
error         TEXT NOT NULL DEFAULT ''
outputs       JSONB           -- the plugin's structured outputs on success
progress_blob JSONB           -- the most recent opaque progress payload from Execute
prompt_blob   JSONB           -- the opaque ExecuteSuspend prompt while awaiting_resolution (schema v4)
PRIMARY KEY (run_id, step_id)
```

Per-step lifecycle records written by the Temporal `RecordStepStart`,
`RecordStepAwaiting`, and `RecordStepFinish` activities, plus in-flight
updates to `progress_blob` from every Execute progress event. This is what lets
the UI show live per-step state (and the `step-runtime` slot's most
recent progress payload) while the workflow is still running instead of
waiting for `PersistResult` at the end.

**Plugins do not write to Postgres through the orchestrator.** Any
plugin that needs persistent state (cached conflict resolutions, draft
artifacts, last-seen commits) is expected to own its own database and
key entries by the identity tuple it received on Execute.
`plugin-generic-go-dep` is the worked example. It runs its own
Postgres (5433 in `docker-compose.yml`) with two tables:

- `step_patches`, keyed by `(workflow_id, step_id, patch_id)`. It is
  updated only from the `workflow-step-config` MFE via plugin actions
  and read on Execute. See the *Per-step patch queue* section above.
- `command_log`, the complete per-`(workflow_run_id, step_id)` command
  history. See *Command history* above.

`plugin-upstream-rebase` runs its own Postgres too (5434). It holds its
commit tables, conflicts, saved resolutions, decisions, rerere cache,
patch queue, and `command_log` (see *upstream-rebase* below).

The run list endpoints add a derived `awaiting_steps` count (steps in
`awaiting_resolution`) to each run summary so dashboards can flag runs
that need input.

---

## Execution model

`orchestrator/internal/wfengine/` wires Temporal.

- **`ExecuteWorkflow`** - the Temporal Workflow function. Validates DAG
  shape, then dispatches each step in a Temporal goroutine that waits
  on its dependencies (Temporal Futures), executes `RunStep`, and
  records the result. Independent branches run concurrently; a failed
  step cascades `skipped` to its descendants but leaves independent
  branches running so a run reports all failures, not just the first.
- **Suspend / resume loop** - each step goroutine runs
  `RecordStepStart → RunStep`; if the plugin suspended, it drops any
  stale resolve signals, runs `RecordStepAwaiting(prompt)`, and waits
  (Temporal `Selector`) on the per-step signal channel
  `resolve-step:<step_id>` or on cancellation. A signal re-runs
  `RecordStepStart → RunStep` with the signal's opaque blob as
  `resolution`; a cancellation marks the step `cancelled`.
- **`RunStep`** - the Activity that gRPC-calls the plugin. Retries
  3× with exponential backoff (5s → 2m), 2 minute heartbeat. Its
  StartToClose timeout is the step's `timeout:` (default 30 minutes).
  Plugin errors with gRPC code `FailedPrecondition` / `InvalidArgument`
  are wrapped as non-retryable. A background goroutine records
  heartbeats while the (opaque) plugin call is in flight and cancels
  the gRPC call if Temporal delivers a cancellation. As progress events
  arrive from the
  plugin's stream, `RunStep` writes the latest one to
  `run_steps.progress_blob` and publishes it on the in-process run-event
  bus so any SSE subscriber gets it immediately.
- **`RecordStepStart`** / **`RecordStepAwaiting`** /
  **`RecordStepFinish`** - best-effort bookkeeping activities called by
  the workflow around each `RunStep` invocation so the live per-step
  state is visible in the UI without paging Temporal history. Short
  timeouts, bounded retries - a dropped record just means a stale UI,
  not a failed run. Finish records are
  written from a Temporal disconnected context so a cancellation
  mid-step still gets a terminal row.
- **`ReleaseStep`** - runs after `RecordStepFinish` for every step
  that reached `RunStep` (succeeded, failed, or cancelled; never for
  `skipped` steps), and calls `Plugin.ReleaseStep`. It uses a
  disconnected context, a 5 minute StartToClose (a release can mean
  deleting a multi-GB clone), and 5 attempts. Failures are ignored.
  The step's future settles *before* the release, so dependents don't
  wait on it. The workflow waits for every release to finish, after
  `PersistResult`, before it returns.
- **`PersistResult`** - the Activity each workflow runs last. Writes
  the final `RunResult` JSON into `run_summaries.results` and sets
  `status` + `completed_at`. Retries 10× because losing this write
  would diverge orchestrator and Temporal records. Runs on a
  disconnected context so it still fires when the workflow was
  cancelled.
- **Cancellation** - `POST /api/runs/:run_id/cancel` calls
  `CancelWorkflow`. In-flight `RunStep` activities see the cancellation
  through the heartbeat goroutine, unblock the plugin gRPC call, and
  the workflow marks any pending / running steps as `cancelled` before
  writing its terminal summary. Steps that were running or suspended
  then get a `ReleaseStep`, so plugins drop their working state.
- **`RecordRunStart`** - a Temporal Schedule fire has no `TriggerRun`
  in front of it, so its input has no run ID and no `run_summaries` row
  exists. `ExecuteWorkflow` then uses the schedule-generated Temporal
  workflow ID (`<workflow-id>-<scheduled time>`) as the run ID and runs
  this activity to open the row.
- **No workflow versioning** - `ExecuteWorkflow` has no
  `GetVersion` gates. Changing the order or set of activities it
  schedules makes runs that are still open in Temporal (running or
  suspended) fail replay, so cancel or purge them before deploying such
  a change.
- **Schedules** - every workflow with a non-empty `schedule:` cron
  expression is registered as a Temporal Schedule (ID
  `upstream-readiness:<workflow-id>`) at startup. The orchestrator
  deletes the schedules it owns that are no longer wanted, and
  recreates every wanted one, because the schedule's arguments embed the
  definition and instance configs as they were at startup.
- **Purge** - `Engine.PurgeWorkflow` (not a Temporal workflow; it runs
  inside the HTTP request) refuses unless the workflow is removed, none
  of its runs is still running (stale summaries are reconciled with
  Temporal first), and every plugin the workflow used is registered.
  Then, in order:
  1. calls `PurgeWorkflow` on every registered plugin; any error stops
     here;
  2. deletes each run's Temporal execution and the workflow's schedule
     (NotFound is fine);
  3. deletes the orchestrator rows.

  Every step is idempotent, so a failed purge is retried by purging
  again.
- **Run-event bus + SSE** - `orchestrator/internal/runbus` is an
  in-process pub/sub keyed by `run_id`. `RunStep`, `RecordStepStart`,
  `RecordStepFinish`, and `PersistResult` publish `step-start`,
  `step-progress`, `step-finish`, and `run-status` events; the HTTP
  layer forwards them to browsers via `GET /api/runs/:run_id/events`
  (Server-Sent Events). `RecordStepAwaiting` publishes `step-awaiting`
  (`{run_id, step_id, prompt}`). Late subscribers bootstrap from a `snapshot`
  frame containing the current summary + `run_steps` rows, then
  receive live events.

Task queue: `upstream-readiness-workflows`.

---

## Repository layout

```text
proto/                                       shared gRPC contract module
  buf.yaml, buf.gen.yaml
  pluginsdk/v1/plugin.proto                  Plugin service definition
  gen/pluginsdk/v1/                          generated Go stubs (committed)

orchestrator/                                Go service: shell UI + Temporal client + worker
  main.go                                    wires storage, registry, Temporal, HTTP server
  internal/registry/registry.go              dials plugins, caches manifests, forwards streaming Execute / Validate / GetAsset / ReleaseStep
  internal/storage/storage.go                Postgres: plugin_instances, workflow_defs, run_summaries, run_steps
  internal/runbus/runbus.go                  in-process pub/sub for run-scoped events (backs SSE)
  internal/workflow/types.go                 Definition / Step / StepResult / RunResult
  internal/workflow/loader.go                YAML loader (normalises step.params into JSON bytes)
  internal/workflow/validate.go              structural DAG validator
  internal/wfengine/engine.go                Engine: connect, worker, TriggerRun, CancelRun, ResolveStep, EnsureSchedules, PurgeWorkflow
  internal/wfengine/workflow.go              ExecuteWorkflow (the Temporal Workflow function)
  internal/wfengine/activity.go              RunStep, PersistResult, RecordRunStart, RecordStepStart, RecordStepAwaiting, RecordStepFinish, ReleaseStep
  internal/wfengine/types.go                 TriggerInput / StepActivityInput / etc.
  internal/httpapi/server.go                 HTTP routes (plugins, plugin MFE assets, instances, workflows, runs, SSE)
  web/                                       Vite + React + PatternFly shell
    index.html                               import map: bare React specifiers → /vendor/*.js
    vite.config.js                           builds the shell + /vendor/ React entries (shared with plugin MFEs)
    src/App.jsx                              shell page + sidebar; selection state
    src/PluginPanel.jsx                      dynamic-imports + mounts a plugin MFE in any mode
    src/InstancesView.jsx                    per-plugin instance CRUD; mounts MFE in instance-config
    src/RunsView.jsx                         cross-workflow runs dashboard (default landing page)
    src/WorkflowsView.jsx                    workflow detail; SSE-driven live DAG + step-runtime slots
    src/WorkflowGraph.jsx                    SVG layout of the DAG (clickable nodes)
    src/pluginApi.js                         the API surface handed to every plugin MFE

plugin-greeter/                              minimal example plugin
  main.go                                    gRPC :9000 (MFE bundle served via GetAsset)
  internal/service/grpc.go                   Plugin contract impl (greet action, suffix instance config)
  web/src/GreeterPanel.jsx                   MFE: action + instance-config modes

pluginkit/                                   shared plugin-side code (not orchestrator code)
  mfeassets/                                 Plugin.GetAsset over the embedded web/dist (used by every plugin)
  gitenv/                                    instance config shape + validation, SSH/git isolation env, URL helpers
  refs/                                      __ref resolution + fork auto-replace detection
  runner/                                    command runner: progress emit, bounded log tail, Recorder hook for every exec
  godeps/                                    per-dir bump → tidy → vendor → AfterVendor hook → make pipeline
  patchqueue/                                step_patches schema/queries, patches_* handlers, ApplyPatch
  shellcmds/                                 step `commands`: validation, sh -c per entry, one commit each
  cmdlog/                                    command_log schema/queries, runner.Recorder, cmdlog_* handlers
  execstream/                                SendFinalJSON / SendSuspendJSON helpers
  web/src/                                   shared JSX: instance form, field helpers, LogTail, PatchQueueEditor, CommandHistory

plugin-generic-go-dep/                       realistic example plugin: bumps a Go module dep
  main.go                                    gRPC :9100 (MFE bundle served via GetAsset)
  internal/service/grpc.go                   manifest, params, commit messages; mechanics from pluginkit
  internal/store/store.go                    plugin-owned Postgres (step_patches via pluginkit/patchqueue)
  web/src/GoDepPanel.jsx                     MFE: action, instance-config, step-runtime, workflow-step-config, …

plugin-upstream-rebase/                      fork rebaser: merge -s ours + re-pick UPSTREAM commits, human-in-the-loop
  main.go                                    gRPC :9200 (MFE bundle served via GetAsset)
  internal/gitrebase/                        git mechanics: classify, find previous rebase, enumerate, pick, conflicts, rerere
  internal/service/rebase.go                 the rebase action (suspend/resume, completeness check, dependency phase)
  internal/service/mfe.go                    MFE-backing actions (run_get, conflict_get, resolution_save, …)
  internal/service/workdir.go                working-copy lifecycle: ReleaseStep, startup sweep
  internal/store/store.go                    plugin-owned Postgres
  web/src/RebasePanel.jsx                    MFE: instance-config, step-runtime (commit table), resolution, workflow-step-config
  hack/make-fixture.sh                       builds a local upstream/fork pair to try the plugin end-to-end

workflows/                                   every *.yaml here is loaded at startup (the directory is the source of truth)
  upstream-rebase.workflow.yaml              "Example Rebase": the example multi-step Go dep chain
  kube-libraries.yaml                        openshift api → client-go → library-go → apiserver-library-go dep chain
  kube-libraries-and-operators.workflow.yaml wider variant of the same chain
  openshift-kubernetes-rebase.workflow.yaml  upstream-rebase step for openshift/kubernetes
  kubernetes-sigs-controller-tools.workflow.yaml  upstream-rebase step for openshift/kubernetes-sigs-controller-tools

docker-compose.yml                           orchestrator postgres + plugin-owned postgres (generic-go-dep, upstream-rebase) + temporal + temporal-ui for local dev
temporal/dynamicconfig/development-sql.yaml  Temporal auto-setup config (empty map → defaults)
```

---

## Prerequisites

- Go 1.25+
- Node 20+
- Docker (for Postgres + Temporal locally)

---

## Running it locally

```bash
# 1. Bring up Postgres + Temporal + Temporal Web UI
docker compose up -d
#   Temporal Web UI:      http://localhost:8233
#   Temporal gRPC:        localhost:7233
#   Postgres (orch.):     postgres://orchestrator:orchestrator@localhost:5432/orchestrator
#   Postgres (go-dep):    postgres://godep:godep@localhost:5433/godep
#     ^ owned by the generic-go-dep plugin (per-step patch queue).
#       The orchestrator has no handle to it. See the "Plugin-owned
#       Postgres" note under generic-go-dep.
#   Postgres (rebase):    postgres://rebase:rebase@localhost:5434/rebase
#     ^ owned by the upstream-rebase plugin.
#   Postgres (temporal):  isolated; managed by the auto-setup image

# 2. Build the web bundles. Each Go binary embeds its web/dist
#    directory at compile time, so you must rebuild dist before
#    rebuilding/restarting the binary if you change UI source.
#    The git plugins also bundle shared JSX from pluginkit/web/src
#    (Vite alias `@pluginkit`), so rebuild both after touching it.
#    The orchestrator bundle includes React (served at /vendor/) and
#    the PatternFly stylesheet, so the UI needs no CDN access.
(cd orchestrator/web        && npm install && npm run build)
(cd plugin-greeter/web      && npm install && npm run build)
(cd plugin-generic-go-dep/web && npm install && npm run build)
(cd plugin-upstream-rebase/web && npm install && npm run build)

# 3. Start the plugin(s). Each plugin is its own process.
cd plugin-greeter && go run . &
cd ../plugin-generic-go-dep && go run . &
cd ../plugin-upstream-rebase && go run . &

# 4. Start the orchestrator, telling it where to find the plugins
#    and where the workflow YAML files live.
cd ../orchestrator && go run . \
  --plugins=localhost:9000,localhost:9100,localhost:9200 \
  --workflows=../workflows
```

Open <http://localhost:8080>. That is the only port the browser uses.
Plugin MFE bundles are fetched through the orchestrator
(`/plugins/{id}/assets/…`), so in a deployment the plugins and
databases can stay on a private network.

For shell UI work, `npm run dev` in `orchestrator/web` starts Vite's
dev server, which proxies `/api` and `/plugins` to `:8080` and serves
`/vendor/*.js` from its own React, so plugin MFEs share the dev shell's
React too.

---

## End-to-end: run a multi-step Go dependency update

This walks through the included example
(`workflows/upstream-rebase.workflow.yaml`), which exercises every part
of the design: one plugin, one instance, four steps in a fan-out / fan-in
shape, and downstream steps binding to upstream outputs.

### What the workflow does

Bump `github.com/upstream-org/core` to `v2.0.0` in `lib-core`, then bump
`lib-utils` and `lib-auth` (in parallel) to the commit `lib-core` was
bumped to, then bump `main-app` to the commit `lib-utils` was bumped to.

```text
   update-lib-core ──┬──► update-lib-utils ──┐
                     │                        ├──► update-main-app
                     └──► update-lib-auth  ──┘
```

The YAML carries minimal per-step content - repo URL, dependency,
version. Git identity, default push remote, and default make targets
live on the instance.

### Step 1 - Create the plugin instance

In the UI:

1. Sidebar → **Instances** → **Go Dependency Updater** → **New
   instance**.
2. The form rendering is the plugin's MFE in `instance-config` mode.
   The orchestrator only sees the resulting blob.
3. Fill in:
   - **Display name:** anything human-readable, e.g. `acme corp bot`
   - **Git user name / email:** the identity to commit as
   - **Fork URL template:** with a `{repo}` placeholder, e.g.
     `git@github.com:acme-bot/{repo}.git`. The plugin substitutes
     `{repo}` with the upstream URL's basename per step to derive the
     push target. Per-step `push_url` overrides this when set.
   - **Default make targets:** comma-separated, e.g. `test`
   - **SSH private key:** PEM contents. Required. Written to a per-run
     temp file at mode 0600; the host's `~/.ssh` is never read.
   - **SSH known_hosts:** recommended. When set, host-key checking is
     strict; otherwise the plugin uses `accept-new` for the run.
   - **Environment variables:** optional. `KEY=value` per line. Set for
     every `git`, `go`, `go mod vendor`, and `make` invocation on top
     of the plugin's isolation defaults. See the "Git auth model"
     section for the list of keys the plugin refuses to let you
     override.
4. Click **Create instance**. The plugin's MFE calls
   `pluginApi.saveInstance(...)`; the orchestrator POSTs to
   `/api/instances` and stores the config blob.

The instance form has no ID field, so the UI assigns a UUID (shown
under the instance's name in the list). Edit the workflow YAML's
`instance:` field for each step to that ID. To keep the bundled YAML
unchanged instead, create the instance through the API with the ID it
expects: `PUT /api/instances/github-acme-bot` with
`{plugin_id, display_name, config_blob}`.

> **Clone-from-upstream / push-to-fork.** The plugin clones from
> `clone_url` (read-only) and pushes the resulting branch to a
> *different* URL (the fork). It refuses to push back to the clone
> source. Pushes are `--force` so Temporal retries / re-runs against
> the same branch name overwrite stale state on the fork rather than
> failing on non-fast-forward.

> Plugin instances are scoped per-plugin: two instances with the same
> ID under different plugins would fail because instance IDs are
> globally unique by design (a workflow step references the instance ID
> alone). The UI always generates a UUID; only the API
> (`POST /api/instances` with an `id`, or `PUT /api/instances/:id`)
> lets you choose one.

### Step 2 - Edit the workflow (optional)

Open `workflows/upstream-rebase.workflow.yaml`. Substitute real Git
URLs for `https://github.com/your-org/lib-core`, etc. If your instance
ID isn't `github-acme-bot`, edit the four `instance:` lines.

The orchestrator upserts this YAML on startup, so to re-load after
edits: restart `go run ./orchestrator`. Workflow files are the only way
to define workflows; there is no API to create or edit them.

### Step 3 - Run

In the UI: **Workflows** → **Example Rebase** → **Run now**.

What happens, in order:

1. UI POSTs `/api/workflows/upstream-rebase/run`. The handler:
   - Validates DAG shape.
   - Resolves every distinct `instance:` referenced by the workflow:
     `Store.GetInstance("github-acme-bot")` → loads the config blob.
     If the instance doesn't exist, returns 422 immediately.
   - Generates a `run_id` (`upstream-rebase-{unix-ms}`).
   - Inserts a `run_summaries` row with `status=running`.
   - Calls `client.ExecuteWorkflow(...)` to start the Temporal
     workflow. Returns `{run_id}` (HTTP 202).
2. The workflow view opens an SSE stream on `/api/runs/:run_id/events`
   and updates the DAG from its events. (The cross-workflow **Runs**
   dashboard polls instead.)
3. Temporal schedules `ExecuteWorkflow`. The workflow:
   - Starts a Temporal goroutine per step.
   - `update-lib-core` has no dependencies; it dispatches `RunStep`
     immediately.
   - `RunStep` calls the plugin's gRPC `Execute("update", ...)` with
     `instance_id="github-acme-bot"`, `instance_config={git_user_name:…}`,
     `payload={clone_url:…, dirs:[{path:".", updates:[{dependency:…, target_version:"v2.0.0"}]}]}`,
     and identity `(run_id, "update-lib-core", "github-acme-bot", attempt=1,
     idempotency_key="<run_id>/update-lib-core")`.
   - Plugin clones the repo, `go get`, `go mod tidy`, `make test`,
     commit, push. Returns `{ref, commit, repo}` as **outputs**.
   - The workflow stores those outputs in its in-memory state for the
     run; they're now available as `upstream_outputs["update-lib-core"]`
     to any step that depends on `update-lib-core`.
   - `update-lib-utils` and `update-lib-auth` were waiting on
     `update-lib-core`'s Future. As soon as it settles, they dispatch
     their own `RunStep` calls in parallel. Each receives
     `upstream_outputs["update-lib-core"] = {ref, commit, repo}`.
     Their `target_version` field is a `__ref` object the plugin
     resolves against that map.
   - `update-main-app` waits on both, then runs with
     `upstream_outputs["update-lib-utils"]` (the YAML only references
     that one).
4. When the last step finishes, the workflow runs `PersistResult`,
   which writes the full `RunResult` JSON into `run_summaries.results`
   and updates `status`.
5. `PersistResult` publishes the terminal `run-status` event; the SSE
   stream delivers it and closes.

The DAG view in **WorkflowsView** colours each step node by its status
(green / red / orange / grey). The right pane shows the per-step status
chips.

### Step 4 - Inspect

In another tab, hit the Temporal UI at <http://localhost:8233> to see
the full workflow history, the per-step Activity invocations, and any
retries. The orchestrator's `/api/runs/:run_id` returns the summarised
`RunResult`.

---

## HTTP API reference

All endpoints are JSON. The orchestrator listens on `:8080` by default.

### Plugins

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/api/plugins` | - | Lists all registered plugin manifests. |
| `POST` | `/api/plugins/:pluginId/execute` | `{ action, payload, instance_id, workflow_id?, step_id?, workflow_run_id? }` | Ad-hoc Execute. `instance_id` is required; the instance must belong to the named plugin. The optional ids are forwarded as the identity tuple so plugin MFEs can scope their own state (`workflow_run_id` defaults to `"ad-hoc"`). The plugin's opaque `result` bytes are returned verbatim. If the plugin suspends, returns 409 (suspension only works inside workflow runs). |
| `POST` | `/api/plugins/:pluginId/validate-instance` | `{ payload }` | Forwards `payload` to the plugin's `Validate(kind="instance")`. Returns `{ valid, errors[] }`. |
| `POST` | `/api/plugins/:pluginId/validate-step` | `{ payload, instance_id?, upstream_outputs? }` | Forwards to `Validate(kind="step")`. Used by the (planned) workflow editor. |

Each manifest's `mfeUrl` in `GET /api/plugins` is the same-origin URL
`/plugins/:pluginId/assets/:mfe_entry`, computed by the orchestrator
from the plugin's `mfe_entry`.

### Plugin MFE assets

Not JSON and not under `/api`: these are the module URLs the shell
dynamic-imports.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/plugins/:pluginId/assets/*path` | Streams the plugin's `GetAsset(path)` response through unchanged. Uses the plugin's `Content-Type` and `ETag`, adds `Cache-Control: no-cache` and `X-Content-Type-Options: nosniff`, and turns `If-None-Match` into a 304. Returns 404 for an unknown plugin or file, 400 for a path that escapes the asset root, and 502 when the plugin is unreachable or errors. |
| `GET` | `/vendor/{react,react-jsx-runtime,react-dom,react-dom-client}.js` | Static. The shell's React, re-exported for plugin MFEs through the import map in `index.html`. |

### Instances

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/api/instances?plugin=:pluginId` | - | Lists all instances (optionally filtered by plugin). Always returns an array (never `null`). |
| `POST` | `/api/instances` | `{ id?, plugin_id, display_name, config_blob }` | Creates an instance. `id` is server-generated if omitted. The plugin's `Validate(kind="instance")` is invoked before persistence. |
| `GET` | `/api/instances/:id` | - | Fetches a single instance. |
| `PUT` | `/api/instances/:id` | `{ plugin_id, display_name, config_blob }` | Upserts. |
| `DELETE` | `/api/instances/:id` | - | Deletes. |

### Workflows

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/api/workflows` | - | Lists all workflow definitions, each with `removed_at` when its file is gone. Read-only: workflows are defined by the files in `--workflows`. |
| `GET` | `/api/workflows/:id` | - | Fetches one workflow definition (plus `removed_at`). |
| `POST` | `/api/workflows/:id/run` | - | Starts an asynchronous run. Returns `{ run_id }` (HTTP 202). 409 if the workflow is removed. |
| `GET` | `/api/workflows/:id/runs` | - | Lists the 50 most recent runs of this workflow. |
| `POST` | `/api/workflows/:id/purge` | - | Deletes every trace of a **removed** workflow (plugin state via `PurgeWorkflow`, Temporal executions + schedule, orchestrator rows). 200 with `{ plugins, temporal_runs, orchestrator }` counts. 409 if it isn't removed, a run is still running, or a plugin it used isn't registered. 502 with `{ error, report }` if a plugin's purge failed; nothing else was deleted, so retry. |

### Runs

| Method | Path | Body | Notes |
|---|---|---|---|
| `GET` | `/api/runs` | - | Lists recent runs across every workflow, newest first. Optional query params: `limit` (default 50, max 500), `status` (`running` \| `succeeded` \| `failed`). Each summary carries `awaiting_steps` (count of steps in `awaiting_resolution`, omitted when 0). Powers the dashboard's cross-workflow Runs view. |
| `GET` | `/api/runs/:run_id` | - | Returns the `RunSummary`. If `status=running`, the handler reconciles with Temporal to detect summaries that are stale because `PersistResult` is in flight. While the run is still going, `results` is synthesised from the `run_steps` table so callers see per-step live state without waiting for `PersistResult`. |
| `GET` | `/api/runs/:run_id/events` | - | Server-Sent Events stream for a single run. First frame is a `snapshot` event carrying the current summary + `run_steps` rows (including `prompt` for awaiting steps); subsequent frames are `step-start`, `step-progress`, `step-awaiting`, `step-finish`, and `run-status`. The stream closes on the terminal `run-status` frame. Keepalive comments every 15s. |
| `POST` | `/api/runs/:run_id/steps/:step_id/resolve` | `{ resolution: <opaque> }` | Answers a suspended step: signals the run's Temporal workflow, which re-executes the step with `ExecuteRequest.resolution` = the blob. 202 on success; 409 if the step isn't `awaiting_resolution`. Called by the shell on behalf of the plugin's `resolution` slot. |
| `POST` | `/api/runs/:run_id/cancel` | - | Cancels a running workflow via Temporal. Returns 202 immediately; the run summary transitions through the normal termination path. Rejects if the run is already terminal. |

---

## Configuration

### Orchestrator

CLI flags (each overrides the corresponding env var):

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--addr` | - | `:8080` | HTTP listen address. |
| `--plugins` | - | `localhost:9000` | Comma-separated list of plugin gRPC endpoints. |
| `--workflows` | - | *(unset)* | Directory of `*.yaml` workflow files to load and upsert on startup. It is the source of truth: after a clean load, stored workflows without a file are flagged removed. A file that fails to load aborts the whole load, and nothing is flagged. |
| `--pg-dsn` | `PG_DSN` | `postgres://orchestrator:orchestrator@localhost:5432/orchestrator?sslmode=disable` | Postgres connection string. |
| `--temporal` | `TEMPORAL_ADDRESS` | `localhost:7233` | Temporal frontend address. |

### Plugins

Every plugin takes:

| Flag | greeter default | go-dep default | rebase default | Purpose |
|---|---|---|---|---|
| `--grpc-addr` | `:9000` | `:9100` | `:9200` | gRPC port the orchestrator dials. This is the plugin's only listener; its MFE bundle is served over it (`GetAsset`), so it only needs to be reachable from the orchestrator. |

(`--http-addr` and `--mfe-url` were removed when MFE delivery moved
behind the orchestrator.)

`generic-go-dep` additionally takes:

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--pg-dsn` | `PG_DSN` | `postgres://godep:godep@localhost:5433/godep?sslmode=disable` | Plugin-private Postgres DSN. Backs the per-`(workflow_id, step_id)` `step_patches` table. |
| `--work-dir` | `GODEP_WORK_DIR` | `$TMPDIR/generic-go-dep` | Holds one `update-*` scratch dir (clone + GOPATH) per in-flight update. Each one is removed when its Execute call returns. The whole dir is emptied at startup to catch runs killed mid-flight, so don't point two plugin processes at the same one. Logged at startup. |

`upstream-rebase` additionally takes:

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--pg-dsn` | `PG_DSN` | `postgres://rebase:rebase@localhost:5434/rebase?sslmode=disable` | Plugin-private Postgres DSN (commit tables, conflicts, resolutions, decisions, rerere cache, patch queue). |
| `--work-dir` | `REBASE_WORK_DIR` | `$TMPDIR/upstream-rebase` | Where a suspended rebase keeps its working copy so a resume continues in place. A cache only: a missing or mismatched copy makes the resume replay from scratch. At startup everything in it is removed except copies of runs still suspended and updated in the last 7 days, so don't point two plugin processes at the same one (see "Working-copy cleanup"). Logged at startup. |

---

## Workflow YAML reference

```yaml
id: upstream-rebase                                # required, unique
display_name: "Example Rebase"                     # required
schedule: "0 6 * * 1"                              # optional cron; empty = manual only

steps:
  - id: update-lib-core                            # required, unique within workflow
    plugin: generic-go-dep                         # required; must be a registered plugin id
    instance: github-acme-bot                      # required; must be an existing instance id
    action: update                                 # required; must match an action the plugin declares
    depends_on: []                                 # optional; defaults to []
    timeout: 30m                                   # optional; per-execution StartToClose (Go duration). Default 30m.
                                                   # Time spent awaiting_resolution doesn't count.
    params:                                        # opaque to the orchestrator
      # anything the plugin's step-editor / hand-author chooses to put here
      clone_url: "git@github.com:your-org/lib-core.git"
      # `dirs` is one entry per Go module directory inside the cloned
      # repo. Use `path: "."` for a single-module repo; add more
      # entries for a monorepo. Each dir owns its own updates,
      # make_targets, and run_vendor decision.
      dirs:
        - path: "."
          updates:
            - dependency: "github.com/upstream-org/core"
              target_version: "v2.0.0"
      # Optional bare shell commands, run in order after every dir's
      # pipeline; each one's changes land in their own commit.
      # commands:
      #   - name: regenerate mocks
      #     run: "go generate ./..."
      #     dir: "."                             # optional, relative to the repo root

  - id: update-main-app
    plugin: generic-go-dep
    instance: github-acme-bot
    action: update
    depends_on: [update-lib-utils, update-lib-auth]
    params:
      clone_url: "git@github.com:your-org/main-app.git"
      dirs:
        - path: "."
          # A single dir can bump many modules at once. Each entry is
          # independently auto-replaced when its target_version binds
          # to an upstream step's commit output. All bumps in a dir
          # land in one commit; all dirs land on the same branch /
          # push.
          updates:
            - dependency: "github.com/your-org/lib-utils"
              target_version:
                __ref: { step: update-lib-utils, output: commit }
            - dependency: "github.com/your-org/lib-auth"
              target_version:
                __ref: { step: update-lib-auth, output: commit }
```

The orchestrator's YAML loader normalises mapping values into JSON
bytes via `gopkg.in/yaml.v3` → re-marshal to JSON, then hands the
plugin those bytes verbatim. The orchestrator never inspects what's
inside `params`.

---

## Regenerating the proto stubs

```bash
go install github.com/bufbuild/buf/cmd/buf@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

cd proto
buf lint
buf generate
```

`buf.yaml` enables breaking-change detection on every commit at the
file level - anyone who changes `plugin.proto` in a breaking way will
get a CI failure (once CI is wired up).

---

## Writing a new plugin

Minimum viable plugin:

1. New Go module whose `main.go` starts one gRPC listener.
2. Implement the `Plugin` RPCs (`ReleaseStep` is optional; embed
   `pluginpb.UnimplementedPluginServer` and skip it if you keep no
   per-step state between Execute calls).
   - `Describe` returns a manifest with a unique `id`, the `mfe_entry`
     path of the MFE module inside `web/dist`, one or more `actions`
     (each with named outputs), and the `mfe_slots` your MFE handles.
   - `Execute` switches on `action`, reads `payload` and
     `instance_config`, optionally walks `upstream_outputs` for
     references, returns `{ result, outputs }`.
   - `Validate` switches on `kind`, returns `{ valid, errors }`.
   - `GetAsset` serves `web/dist`. Delegate to `pluginkit/mfeassets`
     (`mfeassets.New(webDist, "<entry>.js")`, which also supplies
     `Entry()` for `mfe_entry`).
   - `ReleaseStep` drops anything kept for a `(workflow_run_id,
     step_id)` (e.g. a suspended step's working copy). Idempotent.
   - `PurgeWorkflow` (optional) deletes everything kept for a
     `workflow_id` when the user purges a removed workflow. Idempotent.
3. Vite library bundle in `web/` with a React component that switches
   on `props.mode` and renders the right slot. Bundles must
   `external`-ise `react`, `react/jsx-runtime`, `react-dom`, and
   `react-dom/client` (see existing plugin `vite.config.js` files) so
   the shell's import map resolves them to the shell's own `/vendor/`
   copy, one shared instance. Any other import must be bundled or
   relative (relative URLs resolve through the orchestrator's asset
   route); never reference a CDN or the plugin's own host.
4. Embed `web/dist` into the Go binary with `//go:embed all:web/dist`.
5. Add the plugin's gRPC endpoint to the orchestrator's `--plugins`
   flag.

Once registered, the plugin appears in the orchestrator's sidebar and
can be referenced by workflow steps.

If the plugin needs human input mid-step, end the Execute stream with
`ExecuteSuspend` (see *Suspend / resume*), declare the `resolution` slot,
and keep the real answer in the plugin's own store. If it drives git /
Go tooling, build on `pluginkit` rather than copying from an existing
plugin.

---

## Git auth model (generic-go-dep and upstream-rebase plugins)

Both plugins share this model via `pluginkit/gitenv` (and use the same
instance config shape and form). The plugin **only** uses the SSH key
from its instance config; it never
reads the host's `~/.ssh` or `~/.gitconfig`. Per Execute call:

1. The instance's `ssh_private_key` is written to a per-run scratch
   file at mode 0600 inside the temp clone directory.
2. The instance's `ssh_known_hosts` (if set) is written next to it.
3. The repo is cloned into `$GOPATH/src/<module-path>` - not an
   arbitrary flat temp dir - where `<module-path>` is derived from
   `clone_url` the same way fork URLs are translated for auto-replace
   (e.g. `git@github.com:org/repo.git` → `github.com/org/repo`; falls
   back to `$GOPATH/src/local/repo` for URL shapes it can't parse).
   `GOPATH` itself is a subdirectory of the per-run temp dir, set
   explicitly via the `GOPATH` env var rather than left to default from
   `HOME`. This matters because plenty of real-world Makefiles and
   `go:generate` directives (especially in pre-modules or vendored
   repos) resolve tool and import paths by walking up to `$GOPATH/src`,
   and fail with confusing errors when the working copy isn't actually
   there - Go's own module-aware tooling doesn't care where the clone
   lives, so this costs nothing for repos that don't need it.
4. The plugin sets these environment variables for every `git`, `go`,
   and `make` invocation:
   - `GIT_SSH_COMMAND=ssh -i <key> -o UserKnownHostsFile=<known_hosts>
     -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=<…>`
   - `GIT_TERMINAL_PROMPT=0` - never block on a TTY prompt
   - `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_SYSTEM=/dev/null` -
     ignore host gitconfig
   - `HOME=<temp dir>` - last-resort isolation against any tool that
     still resolves `$HOME/.gitconfig` or `$HOME/.ssh`
   - `GOPATH=<temp dir>/gopath` - see point 3 above
   - `GONOSUMDB=*`, `GOSUMDB=off`, `GOFLAGS=-mod=mod` - bypass the
     checksum DB (it hasn't seen private forks or freshly-minted
     pseudo-versions) and let `go get` mutate `go.mod`. Deliberately
     *not* `GOPRIVATE=*`: that also sets `GONOPROXY=*`, which would
     force direct fetches of public vanity import paths that may be
     unreachable from inside the pipeline container. The SSH
     `insteadOf` rewrites below already redirect private-fork
     `https://` URLs to `git@host:`, so the proxy and SSH coexist.
   - `GIT_CONFIG_COUNT` + `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n` -
     one `insteadOf` rewrite per unique git host in play
     (`https://<host>/` → `git@<host>:`) so `go mod tidy`'s internal
     `git clone` uses SSH and picks up the instance's key.
   - Any `env_vars` from the instance config (see below) are merged
     into this environment last, except that the keys above are
     protected from override.
5. The scratch dir (including `GOPATH`, and therefore the Go module
   cache downloaded into it) is removed at the end of the run. Removal
   first walks the tree chmod'ing every directory owner-writable,
   since the Go module cache marks its own entries read-only to guard
   against accidental edits - a plain recursive delete stops at the
   first such directory and leaves the rest on disk. The SSH key is
   never persisted anywhere outside the orchestrator's Postgres
   `plugin_instances` row.

### `env_vars` on the instance config

The instance config carries a `map<string,string>` of extra environment
variables the plugin sets before running `go mod vendor` and any
`make` targets (as well as the underlying `git` / `go` calls, since a
single environment is built once per run). Useful for things like
`GOPROXY=direct`, `CGO_ENABLED=0`, `GO111MODULE=on`, or repo-specific
`MAKEFLAGS`. The plugin refuses to let user-supplied entries override
any of the isolation vars listed above (`GIT_*`, `HOME`, `GOPATH`,
`GOSUMDB`, `GONOSUMDB`, `GOFLAGS`, `GIT_CONFIG_*`) - those exist to
guarantee the plugin's behaviour is reproducible and independent of
the host, and letting an instance config disable them would silently
reintroduce the leaks they're preventing.

If `ssh_known_hosts` is populated, host-key checking is strict
(`StrictHostKeyChecking=yes`). If empty, the plugin uses `accept-new`
which writes any newly-seen host key to the run-scoped scratch file -
strict against changes within the run, no protection against MITM on
the first connection. Populate `ssh_known_hosts` for production use.

> **Note on key storage.** SSH private keys are currently stored
> opaquely in the orchestrator's `plugin_instances.config_blob` JSONB.
> This is fine for development but not adequate for production secret
> hygiene; integration with a real secret store (Vault, sealed-secrets,
> per-plugin DB encrypted at rest) is on the deferred list.

## Known/planned and not done yet

- **Clean AI interface**
- **Auth & Identity**
