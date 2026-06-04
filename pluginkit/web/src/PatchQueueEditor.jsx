// Per-(workflow_id, step_id) patch queue editor - the UI half of
// pluginkit/patchqueue. Lists the step's attached patches, lets the
// author add/edit (name, target dir picked from the step's `dirs`,
// pasted or uploaded patch body), reorder, and delete. Round-trips
// exclusively through pluginApi.execute's patches_* actions with
// workflow_id + step_id scoping; storage lives in the plugin's own DB.
//
// Mount it from a plugin's workflow-step-config slot, passing the slot's
// slotContext through. `intro` optionally replaces the explanatory text.
import { useEffect, useState } from "react";
import {
  ActionGroup,
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
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
} from "@patternfly/react-core";

export function PatchQueueEditor({ pluginApi, slotContext, intro }) {
  const { workflowId, stepId, instanceId, action, params } = slotContext ?? {};

  // The dirs the step is configured to touch. The patch's target_dir
  // has to be one of these - patching a dir that isn't in the step's
  // `dirs` list would silently do nothing at Execute time.
  const configuredDirs = (params?.dirs ?? [])
    .map((d) => (d && d.path ? String(d.path) : ""))
    .filter(Boolean);

  const [patches, setPatches] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [savingId, setSavingId] = useState(null);
  const [showAdd, setShowAdd] = useState(false);

  const canPersist = Boolean(workflowId && stepId && instanceId);

  const reload = async () => {
    if (!canPersist) return;
    setLoading(true);
    setError(null);
    try {
      const res = await pluginApi.execute("patches_list", {}, {
        instanceId, workflowId, stepId,
      });
      setPatches(res?.patches ?? []);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workflowId, stepId, instanceId]);

  const savePatch = async (input) => {
    setSavingId(input.patch_id ?? "__new__");
    setError(null);
    try {
      await pluginApi.execute("patches_upsert", input, { instanceId, workflowId, stepId });
      await reload();
      return true;
    } catch (e) {
      setError(String(e));
      return false;
    } finally {
      setSavingId(null);
    }
  };

  const deletePatch = async (patchId) => {
    if (!window.confirm("Delete this patch? This cannot be undone.")) return;
    setSavingId(patchId);
    setError(null);
    try {
      await pluginApi.execute("patches_delete", { patch_id: patchId }, { instanceId, workflowId, stepId });
      await reload();
    } catch (e) {
      setError(String(e));
    } finally {
      setSavingId(null);
    }
  };

  const movePatch = async (idx, delta) => {
    const next = idx + delta;
    if (next < 0 || next >= patches.length) return;
    const ordered = patches.map((p) => p.patch_id);
    [ordered[idx], ordered[next]] = [ordered[next], ordered[idx]];
    setError(null);
    try {
      const res = await pluginApi.execute(
        "patches_reorder",
        { ordered_patch_ids: ordered },
        { instanceId, workflowId, stepId },
      );
      setPatches(res?.patches ?? []);
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="small">
          {intro ?? (
            <>
              Per-workflow configuration for step <code>{stepId}</code>. Patches attached
              here are applied after <code>go mod vendor</code> and
              before the <code>make</code> targets for a given target dir for every run of
              workflow <code>{workflowId}</code>.
            </>
          )}
        </Content>
      </StackItem>

      <StackItem>
        <DescriptionList isCompact>
          <DescriptionListGroup>
            <DescriptionListTerm>Workflow</DescriptionListTerm>
            <DescriptionListDescription><code>{workflowId}</code></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Step</DescriptionListTerm>
            <DescriptionListDescription><code>{stepId}</code></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Action</DescriptionListTerm>
            <DescriptionListDescription><code>{action}</code></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Instance</DescriptionListTerm>
            <DescriptionListDescription><code>{instanceId}</code></DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      </StackItem>

      {configuredDirs.length === 0 && (
        <StackItem>
          <Alert variant="warning" isInline title="No dirs on this step">
            The step's <code>params.dirs</code> list is empty, so there is nowhere
            to attach a patch. Add at least one dir to the workflow YAML first.
          </Alert>
        </StackItem>
      )}

      {error && (
        <StackItem>
          <Alert variant="danger" isInline title="Patch operation failed">
            <pre style={{ margin: 0, fontSize: 12, whiteSpace: "pre-wrap" }}>{error}</pre>
          </Alert>
        </StackItem>
      )}

      <StackItem>
        <Card>
          <CardTitle>
            <Split hasGutter style={{ alignItems: "center" }}>
              <SplitItem isFilled>Attached patches ({patches.length})</SplitItem>
              <SplitItem>
                <Button
                  variant="secondary"
                  isDisabled={configuredDirs.length === 0 || showAdd}
                  onClick={() => setShowAdd(true)}
                >
                  + Add patch
                </Button>
              </SplitItem>
            </Split>
          </CardTitle>
          <CardBody>
            {loading ? (
              <em style={{ color: "#6a6e73" }}>Loading…</em>
            ) : patches.length === 0 && !showAdd ? (
              <em style={{ color: "#6a6e73" }}>
                No patches attached. Add one to apply a canned diff every time this step runs.
              </em>
            ) : (
              <Stack hasGutter>
                {patches.map((p, idx) => (
                  <StackItem key={p.patch_id}>
                    <PatchRow
                      patch={p}
                      idx={idx}
                      total={patches.length}
                      configuredDirs={configuredDirs}
                      onMove={movePatch}
                      onDelete={() => deletePatch(p.patch_id)}
                      onSave={savePatch}
                      saving={savingId === p.patch_id}
                    />
                  </StackItem>
                ))}
                {showAdd && (
                  <StackItem>
                    <PatchRow
                      patch={null}
                      idx={patches.length}
                      total={patches.length + 1}
                      configuredDirs={configuredDirs}
                      onCancel={() => setShowAdd(false)}
                      onSave={async (input) => {
                        const ok = await savePatch(input);
                        if (ok) setShowAdd(false);
                        return ok;
                      }}
                      saving={savingId === "__new__"}
                    />
                  </StackItem>
                )}
              </Stack>
            )}
          </CardBody>
        </Card>
      </StackItem>
    </Stack>
  );
}

// PatchRow renders one patch (existing or new) with inline edit. Kept
// self-contained so the parent's `patches` array stays authoritative -
// the row only proposes a save; the parent decides when to reload.
function PatchRow({
  patch,
  idx,
  total,
  configuredDirs,
  onMove,
  onDelete,
  onCancel,
  onSave,
  saving,
}) {
  const isNew = patch == null;
  const [editing, setEditing] = useState(isNew);
  const [form, setForm] = useState(() => ({
    name: patch?.name ?? "",
    target_dir: patch?.target_dir ?? configuredDirs[0] ?? "",
    content: patch?.content ?? "",
  }));

  useEffect(() => {
    if (!isNew) {
      setForm({
        name: patch?.name ?? "",
        target_dir: patch?.target_dir ?? configuredDirs[0] ?? "",
        content: patch?.content ?? "",
      });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [patch?.updated_at, patch?.patch_id]);

  const canSave =
    form.name.trim().length > 0 &&
    form.target_dir.trim().length > 0 &&
    form.content.trim().length > 0 &&
    configuredDirs.includes(form.target_dir);

  const onFile = async (e) => {
    const file = e.target?.files?.[0];
    if (!file) return;
    const text = await file.text();
    setForm((f) => ({
      ...f,
      content: text,
      name: f.name || file.name.replace(/\.patch$|\.diff$/i, ""),
    }));
    // Reset so the same file can be re-picked after editing.
    e.target.value = "";
  };

  const submit = async () => {
    const input = {
      patch_id: patch?.patch_id,
      name: form.name.trim(),
      target_dir: form.target_dir.trim(),
      content: form.content,
    };
    const ok = await onSave(input);
    if (ok && !isNew) setEditing(false);
  };

  if (!editing) {
    return (
      <Card isCompact>
        <CardBody>
          <Split hasGutter style={{ alignItems: "center" }}>
            <SplitItem>
              <Stack>
                <StackItem>
                  <Button
                    variant="plain"
                    aria-label="Move up"
                    isDisabled={idx === 0 || saving}
                    onClick={() => onMove(idx, -1)}
                  >
                    ▲
                  </Button>
                </StackItem>
                <StackItem>
                  <Button
                    variant="plain"
                    aria-label="Move down"
                    isDisabled={idx === total - 1 || saving}
                    onClick={() => onMove(idx, +1)}
                  >
                    ▼
                  </Button>
                </StackItem>
              </Stack>
            </SplitItem>
            <SplitItem isFilled>
              <Stack>
                <StackItem>
                  <Split hasGutter style={{ alignItems: "center" }}>
                    <SplitItem>
                      <Label isCompact color="grey">#{idx + 1}</Label>
                    </SplitItem>
                    <SplitItem isFilled>
                      <strong>{patch.name}</strong>{" "}
                      <span style={{ color: "#6a6e73", fontSize: 12 }}>
                        → <code>{patch.target_dir}</code>
                      </span>
                    </SplitItem>
                  </Split>
                </StackItem>
                <StackItem>
                  <Content component="small" style={{ color: "#6a6e73" }}>
                    {patch.content.split("\n").length} line(s) · updated {new Date(patch.updated_at).toLocaleString()}
                  </Content>
                </StackItem>
              </Stack>
            </SplitItem>
            <SplitItem>
              <Button variant="link" onClick={() => setEditing(true)} isDisabled={saving}>
                Edit
              </Button>
              <Button variant="link" isDanger onClick={onDelete} isDisabled={saving}>
                Delete
              </Button>
            </SplitItem>
          </Split>
        </CardBody>
      </Card>
    );
  }

  return (
    <Card isCompact>
      <CardTitle>
        {isNew ? "New patch" : `Edit patch #${idx + 1}`}
      </CardTitle>
      <CardBody>
        <Form>
          <FormGroup label="Name" isRequired fieldId={`p-name-${patch?.patch_id ?? "new"}`}>
            <TextInput
              id={`p-name-${patch?.patch_id ?? "new"}`}
              value={form.name}
              onChange={(_, v) => setForm((f) => ({ ...f, name: v }))}
              placeholder="e.g. fix build tag for go1.24"
              aria-label="Patch name"
            />
          </FormGroup>
          <FormGroup label="Target dir" isRequired fieldId={`p-dir-${patch?.patch_id ?? "new"}`}>
            <select
              id={`p-dir-${patch?.patch_id ?? "new"}`}
              value={form.target_dir}
              onChange={(e) => setForm((f) => ({ ...f, target_dir: e.target.value }))}
              style={{ width: "100%", padding: "6px 8px", borderRadius: 4 }}
            >
              {configuredDirs.map((dir) => (
                <option key={dir} value={dir}>{dir === "." ? ".  (root)" : dir}</option>
              ))}
            </select>
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  Must be one of the step's configured dirs. The patch runs after{" "}
                  <code>go mod vendor</code> and before <code>make</code> in this dir.
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Patch content" isRequired fieldId={`p-content-${patch?.patch_id ?? "new"}`}>
            <TextArea
              id={`p-content-${patch?.patch_id ?? "new"}`}
              value={form.content}
              onChange={(_, v) => setForm((f) => ({ ...f, content: v }))}
              placeholder={"diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ ..."}
              rows={12}
              resizeOrientation="vertical"
              aria-label="Patch content"
              style={{ fontFamily: "monospace", fontSize: 12 }}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  Either a plain unified diff (applied via <code>git apply --3way</code>)
                  or a full <code>git format-patch</code> mbox (applied via{" "}
                  <code>git am --3way</code>). File paths are resolved from the repo root.
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Upload from file" fieldId={`p-file-${patch?.patch_id ?? "new"}`}>
            <input
              id={`p-file-${patch?.patch_id ?? "new"}`}
              type="file"
              accept=".patch,.diff,text/plain"
              onChange={onFile}
            />
          </FormGroup>
          <ActionGroup>
            <Button
              variant="primary"
              onClick={submit}
              isLoading={saving}
              isDisabled={saving || !canSave}
            >
              {isNew ? "Add patch" : "Save changes"}
            </Button>
            {isNew ? (
              <Button variant="link" onClick={onCancel} isDisabled={saving}>Cancel</Button>
            ) : (
              <Button variant="link" onClick={() => setEditing(false)} isDisabled={saving}>Cancel</Button>
            )}
          </ActionGroup>
        </Form>
      </CardBody>
    </Card>
  );
}
