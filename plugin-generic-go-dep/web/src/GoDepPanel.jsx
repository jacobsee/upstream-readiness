// generic-go-dep MFE. One component, multiple slots - the orchestrator
// mounts it with a `mode` prop telling it which slot it lives in:
//
//   mode="action"           per-action runtime panel (default)
//   mode="instance-config"  create/edit a plugin instance
//
// The instance form, field helpers, and patch-queue editor are shared
// with the upstream-rebase plugin via pluginkit/web (the `@pluginkit`
// Vite alias).
//
// All persistence is delegated back through pluginApi. The orchestrator
// stores whatever blob this component hands it.

import { useState, useEffect } from "react";
import {
  ActionGroup,
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Checkbox,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Label,
  Split,
  SplitItem,
  Stack,
  StackItem,
  TextArea,
  TextInput,
  Title,
} from "@patternfly/react-core";
import { Field } from "@pluginkit/fields.jsx";
import { GitInstanceConfigForm, GitInstanceSummary } from "@pluginkit/GitInstanceConfigForm.jsx";
import { PatchQueueEditor } from "@pluginkit/PatchQueueEditor.jsx";
import { CommandHistory } from "@pluginkit/CommandHistory.jsx";

export default function GoDepPanel(props) {
  const mode = props.mode || "action";
  if (mode === "instance-config") return <InstanceConfigPanel {...props} />;
  if (mode === "step-runtime") return <StepRuntimePanel {...props} />;
  if (mode === "workflow-step-config") return <WorkflowStepConfigPanel {...props} />;
  if (mode === "instance-list-item") return <InstanceListItemPanel {...props} />;
  if (mode === "sidebar-badge") return <SidebarBadgePanel {...props} />;
  if (mode === "plugin-home") return <HomePanel {...props} />;
  return <ActionPanel {...props} />;
}

// ── step-runtime slot ─────────────────────────────────────────────────────────
// The workflow view mounts this while a step runs and (under the run's
// "Details") after it ends. slotContext.progress is the latest opaque
// payload the plugin's Execute stream emitted - for this plugin,
// { phase, message, log } where log is only a bounded tail. The complete
// history of every command and its output comes from the plugin's DB
// (cmdlog_list / cmdlog_get), live while running.
function StepRuntimePanel({ pluginApi, slotContext }) {
  const p = slotContext?.progress;
  const running = slotContext?.status === "running";
  return (
    <Stack hasGutter>
      {p && (
        <StackItem>
          <Split hasGutter style={{ alignItems: "center" }}>
            <SplitItem><Label color="blue" isCompact>{p.phase || "running"}</Label></SplitItem>
            <SplitItem isFilled><code style={{ fontSize: 12 }}>{p.message}</code></SplitItem>
          </Split>
        </StackItem>
      )}
      {!p && running && (
        <StackItem><em style={{ color: "#6a6e73" }}>Waiting for the plugin to emit progress…</em></StackItem>
      )}
      <StackItem>
        <CommandHistory pluginApi={pluginApi} scope={slotContext} liveKey={running ? p : undefined} />
      </StackItem>
    </Stack>
  );
}

// ── workflow-step-config slot ─────────────────────────────────────────────────
// Mounted when the user clicks a step node in the DAG. Owns the
// per-(workflow_id, step_id) patch queue: an ordered list of git patches
// that handleUpdate applies to each configured dir AFTER `go mod vendor`
// and BEFORE the make targets for that dir. Persistence lives in the
// plugin's own Postgres (see internal/store); this UI round-trips through
// pluginApi.execute's patches_* actions.
function WorkflowStepConfigPanel(props) {
  return <PatchQueueEditor {...props} />;
}

// ── instance-list-item slot ───────────────────────────────────────────────────
// Rendered inside the Instances view's row for this instance so the
// plugin can surface its own summary line without the orchestrator
// parsing config_blob.
function InstanceListItemPanel(props) {
  return <GitInstanceSummary {...props} />;
}

// ── sidebar-badge slot ────────────────────────────────────────────────────────
function SidebarBadgePanel({ pluginApi }) {
  const [count, setCount] = useState(null);
  useEffect(() => {
    let cancelled = false;
    pluginApi.listInstances().then((rows) => { if (!cancelled) setCount(rows.length); }).catch(() => {});
    return () => { cancelled = true; };
  }, [pluginApi]);
  if (count == null) return null;
  return <span style={{ fontSize: 11, color: "#6a6e73" }}>{count}</span>;
}

// ── plugin-home slot ──────────────────────────────────────────────────────────
function HomePanel({ manifest, pluginApi }) {
  const [instances, setInstances] = useState([]);
  useEffect(() => { pluginApi.listInstances().then(setInstances).catch(() => {}); }, [pluginApi]);
  return (
    <Stack hasGutter>
      <StackItem>
        <Title headingLevel="h1" size="2xl">{manifest.displayName}</Title>
        <Content component="small">
          Bumps Go module dependencies, force-pushes to a fork.
          Plugin id <code>{manifest.id}</code>, version {manifest.version}.
        </Content>
      </StackItem>
      <StackItem>
        <Card>
          <CardTitle>Instances ({instances.length})</CardTitle>
          <CardBody>
            {instances.length === 0 ? (
              <em style={{ color: "#6a6e73" }}>No instances yet. Create one under Instances.</em>
            ) : (
              <ul style={{ margin: 0, paddingLeft: 20 }}>
                {instances.map((i) => (
                  <li key={i.id}>
                    <strong>{i.display_name || i.id}</strong>
                    {i.config_blob?.git_user_email && (
                      <span style={{ color: "#6a6e73" }}> - {i.config_blob.git_user_email}</span>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </CardBody>
        </Card>
      </StackItem>
      {instances.length > 0 && (
        <StackItem>
          <Card>
            <CardTitle>Ad-hoc run history</CardTitle>
            <CardBody>
              <Content component="small" style={{ marginBottom: 8 }}>
                Every command run by updates invoked from the action panel (outside a workflow), across
                all instances. Workflow runs keep their history under each step's details.
              </Content>
              <CommandHistory
                pluginApi={pluginApi}
                scope={{ instanceId: instances[0].id, runId: "ad-hoc", stepId: "ad-hoc" }}
              />
            </CardBody>
          </Card>
        </StackItem>
      )}
    </Stack>
  );
}

// ── instance config slot ──────────────────────────────────────────────────────
// Shared with every plugin using pluginkit's gitenv.InstanceConfig shape.
function InstanceConfigPanel(props) {
  return <GitInstanceConfigForm {...props} newTitle="New Go Dependency Updater instance" />;
}

// ── action runtime slot ───────────────────────────────────────────────────────

const EMPTY_UPDATE = { dependency: "", target_version: "" };
const emptyDir = () => ({
  path: ".",
  updates: [{ ...EMPTY_UPDATE }],
  make_targets: "",
  run_vendor: false,
});
const EMPTY_ACTION = {
  instance_id: "",
  clone_url: "",
  push_url: "",
  branch_name: "",
  dirs: [emptyDir()],
};

function ActionPanel({ pluginApi }) {
  const [instances, setInstances] = useState([]);
  const [form, setForm] = useState(EMPTY_ACTION);
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  const [showOutput, setShowOutput] = useState(false);

  useEffect(() => {
    pluginApi
      .listInstances()
      .then((rows) => {
        setInstances(rows);
        if (rows.length > 0 && !form.instance_id) {
          setForm((f) => ({ ...f, instance_id: rows[0].id }));
        }
      })
      .catch((e) => setError(String(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const set = (k) => (v) => setForm((f) => ({ ...f, [k]: v }));

  const setDir = (di, field) => (v) =>
    setForm((f) => ({
      ...f,
      dirs: f.dirs.map((d, i) => (i === di ? { ...d, [field]: v } : d)),
    }));

  const addDir = () => setForm((f) => ({ ...f, dirs: [...f.dirs, emptyDir()] }));
  const removeDir = (di) => setForm((f) => ({ ...f, dirs: f.dirs.filter((_, i) => i !== di) }));

  const setUpdate = (di, ui, field) => (v) =>
    setForm((f) => ({
      ...f,
      dirs: f.dirs.map((d, i) =>
        i === di
          ? { ...d, updates: d.updates.map((u, j) => (j === ui ? { ...u, [field]: v } : u)) }
          : d,
      ),
    }));

  const addUpdate = (di) =>
    setForm((f) => ({
      ...f,
      dirs: f.dirs.map((d, i) => (i === di ? { ...d, updates: [...d.updates, { ...EMPTY_UPDATE }] } : d)),
    }));
  const removeUpdate = (di, ui) =>
    setForm((f) => ({
      ...f,
      dirs: f.dirs.map((d, i) =>
        i === di ? { ...d, updates: d.updates.filter((_, j) => j !== ui) } : d,
      ),
    }));

  const dirsReady =
    form.dirs.length > 0 &&
    form.dirs.every(
      (d) =>
        d.path.trim() &&
        d.updates.length > 0 &&
        d.updates.every((u) => u.dependency && u.target_version),
    );

  const handleSubmit = async (e) => {
    e.preventDefault();
    setRunning(true);
    setResult(null);
    setError(null);
    setShowOutput(false);
    try {
      const params = {
        clone_url: form.clone_url,
        push_url: form.push_url || undefined,
        branch_name: form.branch_name || undefined,
        dirs: form.dirs.map((d) => ({
          path: d.path.trim(),
          updates: d.updates.map((u) => ({ dependency: u.dependency, target_version: u.target_version })),
          make_targets: d.make_targets
            ? d.make_targets.split(",").map((s) => s.trim()).filter(Boolean)
            : [],
          run_vendor: d.run_vendor,
        })),
      };
      const res = await pluginApi.execute("update", params, { instanceId: form.instance_id });
      setResult(res);
    } catch (err) {
      setError(String(err));
    } finally {
      setRunning(false);
    }
  };

  if (instances.length === 0) {
    return (
      <Alert variant="info" title="No instances registered" isInline>
        Create a Go Dependency Updater instance under the Instances tab before invoking actions ad-hoc.
      </Alert>
    );
  }

  return (
    <Stack hasGutter>
      <StackItem>
        <Card>
          <CardTitle>Run an ad-hoc update</CardTitle>
          <CardBody>
            <Form onSubmit={handleSubmit}>
              <FormGroup label="Instance" isRequired fieldId="instance-id">
                <select
                  id="instance-id"
                  value={form.instance_id}
                  onChange={(e) => set("instance_id")(e.target.value)}
                  style={{ width: "100%", padding: "6px 8px", borderRadius: 4 }}
                >
                  {instances.map((i) => (
                    <option key={i.id} value={i.id}>
                      {i.display_name || i.id}
                    </option>
                  ))}
                </select>
              </FormGroup>
              <Field
                label="Upstream URL (clone from)"
                id="clone-url"
                value={form.clone_url}
                onChange={set("clone_url")}
                placeholder="git@github.com:upstream-org/lib-core.git"
                helper="The repo to clone and modify. Read-only - we never push here."
                required
              />
              <Field
                label="Push URL (fork)"
                id="push-url"
                value={form.push_url}
                onChange={set("push_url")}
                placeholder="leave blank to derive from the instance's fork_url_template"
                helper="Overrides the instance's fork_url_template. The new branch is pushed here with --force."
              />

              <Field
                label="Branch name"
                id="branch-name"
                value={form.branch_name}
                onChange={set("branch_name")}
                placeholder="auto-generated"
                helper="Optional. Auto-generated from all bumps across all dirs when omitted."
              />

              <FormGroup label="Module directories" isRequired fieldId="dirs">
                <Stack hasGutter>
                  {form.dirs.map((d, di) => (
                    <StackItem key={di}>
                      <Card isCompact>
                        <CardTitle>
                          <Split hasGutter style={{ alignItems: "center" }}>
                            <SplitItem isFilled>
                              Dir {di + 1} - <code>{d.path || "?"}</code>
                            </SplitItem>
                            <SplitItem>
                              <Button
                                variant="link"
                                isDanger
                                isDisabled={form.dirs.length === 1}
                                onClick={() => removeDir(di)}
                              >
                                Remove dir
                              </Button>
                            </SplitItem>
                          </Split>
                        </CardTitle>
                        <CardBody>
                          <Stack hasGutter>
                            <StackItem>
                              <TextInput
                                id={`dir-path-${di}`}
                                value={d.path}
                                onChange={(_, v) => setDir(di, "path")(v)}
                                placeholder="."
                                aria-label={`Dir ${di + 1} path`}
                              />
                              <FormHelperText>
                                <HelperText>
                                  <HelperTextItem>
                                    Relative to the repo root. Use <code>.</code> for the root module.
                                  </HelperTextItem>
                                </HelperText>
                              </FormHelperText>
                            </StackItem>
                            <StackItem>
                              <Checkbox
                                id={`run-vendor-${di}`}
                                label="Run `go mod vendor` after bumping this dir"
                                isChecked={d.run_vendor}
                                onChange={(_, v) => setDir(di, "run_vendor")(v)}
                              />
                            </StackItem>
                            <StackItem>
                              <TextInput
                                id={`make-targets-${di}`}
                                value={d.make_targets}
                                onChange={(_, v) => setDir(di, "make_targets")(v)}
                                placeholder="test, lint, build"
                                aria-label={`Dir ${di + 1} make targets`}
                              />
                              <FormHelperText>
                                <HelperText>
                                  <HelperTextItem>
                                    Make targets, comma-separated. Run with cwd = this dir. Each target's file changes are committed separately. Empty falls back to the instance's default make targets.
                                  </HelperTextItem>
                                </HelperText>
                              </FormHelperText>
                            </StackItem>
                            <StackItem>
                              <strong style={{ fontSize: 13 }}>Dependencies to bump</strong>
                              <Stack hasGutter style={{ marginTop: 6 }}>
                                {d.updates.map((u, ui) => (
                                  <StackItem key={ui}>
                                    <Split hasGutter style={{ alignItems: "flex-end" }}>
                                      <SplitItem isFilled>
                                        <TextInput
                                          id={`dep-${di}-${ui}`}
                                          value={u.dependency}
                                          onChange={(_, v) => setUpdate(di, ui, "dependency")(v)}
                                          placeholder="github.com/org/lib"
                                          aria-label={`Dir ${di + 1} dependency ${ui + 1}`}
                                        />
                                      </SplitItem>
                                      <SplitItem isFilled>
                                        <TextInput
                                          id={`ver-${di}-${ui}`}
                                          value={u.target_version}
                                          onChange={(_, v) => setUpdate(di, ui, "target_version")(v)}
                                          placeholder="v1.2.3"
                                          aria-label={`Dir ${di + 1} target version ${ui + 1}`}
                                        />
                                      </SplitItem>
                                      <SplitItem>
                                        <Button
                                          variant="link"
                                          isDanger
                                          isDisabled={d.updates.length === 1}
                                          onClick={() => removeUpdate(di, ui)}
                                        >
                                          Remove
                                        </Button>
                                      </SplitItem>
                                    </Split>
                                  </StackItem>
                                ))}
                                <StackItem>
                                  <Button variant="link" onClick={() => addUpdate(di)}>
                                    + Add another dependency
                                  </Button>
                                </StackItem>
                              </Stack>
                            </StackItem>
                          </Stack>
                        </CardBody>
                      </Card>
                    </StackItem>
                  ))}
                  <StackItem>
                    <Button variant="link" onClick={addDir}>
                      + Add another dir
                    </Button>
                  </StackItem>
                </Stack>
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>
                      One entry per Go module dir inside the repo. All dirs land on the same branch, force-pushed to the fork.
                    </HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FormGroup>

              <ActionGroup>
                <Button
                  type="submit"
                  variant="primary"
                  isLoading={running}
                  isDisabled={running || !form.instance_id || !form.clone_url || !dirsReady}
                >
                  {running ? "Running…" : "Update dependencies"}
                </Button>
              </ActionGroup>
            </Form>
          </CardBody>
        </Card>
      </StackItem>

      {error && (
        <StackItem>
          <Alert variant="danger" title="Update failed" isInline>
            <pre style={{ margin: 0, fontSize: 12, whiteSpace: "pre-wrap" }}>{error}</pre>
          </Alert>
        </StackItem>
      )}

      {result && (
        <StackItem>
          <Card>
            <CardTitle>
              <Split hasGutter style={{ alignItems: "center" }}>
                <SplitItem isFilled>Result</SplitItem>
                <SplitItem>
                  <Label color="green">success</Label>
                </SplitItem>
              </Split>
            </CardTitle>
            <CardBody>
              <Stack hasGutter>
                <StackItem>
                  <strong>Branch pushed:</strong>{" "}
                  <code>{result.ref}</code>
                </StackItem>
                <StackItem>
                  <strong>Commit:</strong>{" "}
                  <code style={{ fontSize: 12 }}>{result.commit}</code>
                </StackItem>
                <StackItem>
                  <strong>Fork:</strong>{" "}
                  <code style={{ fontSize: 12 }}>{result.fork_url}</code>
                </StackItem>
                {result.bumps && result.bumps.length > 0 && (
                  <StackItem>
                    <strong>Bumps applied:</strong>
                    <ul style={{ margin: "6px 0 0 20px", fontSize: 12 }}>
                      {result.bumps.map((b, i) => (
                        <li key={i}>
                          <code>{b.path || "."}</code>: <code>{b.dependency}</code> → <code>{b.target_version}</code>
                          {b.replace && (
                            <span style={{ color: "#c9190b" }}>
                              {" "}(replace → <code>{b.replace.fork_module_path}</code> @{" "}
                              <code>{b.replace.fork_commit.slice(0, 12)}</code>)
                            </span>
                          )}
                        </li>
                      ))}
                    </ul>
                  </StackItem>
                )}
                <StackItem>
                  <Checkbox
                    id="show-output"
                    label="Show command output"
                    isChecked={showOutput}
                    onChange={(_, v) => setShowOutput(v)}
                  />
                  {showOutput && result.output && (
                    <pre
                      style={{
                        marginTop: 8,
                        fontSize: 12,
                        background: "#1b1d21",
                        color: "#d4d4d4",
                        padding: 16,
                        borderRadius: 4,
                        overflow: "auto",
                        maxHeight: 400,
                      }}
                    >
                      {result.output}
                    </pre>
                  )}
                </StackItem>
              </Stack>
            </CardBody>
          </Card>
        </StackItem>
      )}
    </Stack>
  );
}
