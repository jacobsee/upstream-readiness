import { useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  DataList,
  DataListCell,
  DataListItem,
  DataListItemCells,
  DataListItemRow,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  EmptyState,
  EmptyStateBody,
  ExpandableSection,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Spinner,
  Split,
  SplitItem,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";
import WorkflowGraph from "./WorkflowGraph.jsx";
import PluginPanel from "./PluginPanel.jsx";
import RunLaunchModal, { initialInputs, rerunPlan } from "./RunLaunchModal.jsx";

// Single-workflow detail view. Fetches the workflow's run history, opens
// an SSE stream to the currently-selected run so the DAG and per-step
// progress panels update without polling, and lets the user click a
// DAG node to open the plugin's workflow-step-config MFE slot.
export default function WorkflowsView({ workflow, plugins, onPurged }) {
  const removed = Boolean(workflow.removed_at);
  const [purgeOpen, setPurgeOpen] = useState(false);
  const [runs, setRuns] = useState(null);
  const [runsError, setRunsError] = useState(null);
  const [selectedRunID, setSelectedRunID] = useState(null);
  const [snapshot, setSnapshot] = useState(null); // { summary, stepsByID }
  const [runError, setRunError] = useState(null);
  const [triggering, setTriggering] = useState(false);
  const [openStepID, setOpenStepID] = useState(null);
  // launch: null, { kind: "run" }, or { kind: "rerun", fromStep, plan,
  // sourceRunID, sourceInputs } while the RunLaunchModal is open.
  const [launch, setLaunch] = useState(null);
  const esRef = useRef(null);

  const pluginById = useMemo(
    () => Object.fromEntries((plugins ?? []).map((p) => [p.id, p])),
    [plugins],
  );

  const loadRuns = async () => {
    try {
      const r = await fetch(`/api/workflows/${encodeURIComponent(workflow.id)}/runs`);
      if (!r.ok) throw new Error(`/api/workflows/${workflow.id}/runs → ${r.status}`);
      const data = (await r.json()) ?? [];
      setRuns(data);
      return data;
    } catch (e) {
      setRunsError(String(e));
      return null;
    }
  };

  useEffect(() => {
    setRuns(null);
    setSelectedRunID(null);
    setSnapshot(null);
    setRunError(null);
    setRunsError(null);
    loadRuns().then((rs) => {
      if (rs && rs.length > 0) setSelectedRunID(rs[0].run_id);
    });
    return () => {
      if (esRef.current) esRef.current.close();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workflow.id]);

  // Open the SSE stream for the selected run. The initial "snapshot"
  // event seeds our per-step map; subsequent step-start/progress/finish
  // and run-status events mutate it live. Terminal status closes the
  // stream and triggers a run-list refresh.
  useEffect(() => {
    if (esRef.current) {
      esRef.current.close();
      esRef.current = null;
    }
    setSnapshot(null);
    setRunError(null);
    if (!selectedRunID) return;

    const es = new EventSource(`/api/runs/${encodeURIComponent(selectedRunID)}/events`);
    esRef.current = es;

    es.addEventListener("snapshot", (e) => {
      try {
        const data = JSON.parse(e.data);
        const stepsByID = {};
        (data.steps ?? []).forEach((s) => {
          stepsByID[s.step_id] = s;
        });
        setSnapshot({ summary: data.summary, stepsByID });
      } catch (err) {
        setRunError(String(err));
      }
    });

    es.addEventListener("step-start", (e) => {
      const d = JSON.parse(e.data);
      setSnapshot((prev) => {
        if (!prev) return prev;
        const existing = prev.stepsByID[d.step_id] ?? {};
        return {
          ...prev,
          stepsByID: {
            ...prev.stepsByID,
            [d.step_id]: { ...existing, step_id: d.step_id, plugin: d.plugin, action: d.action, status: "running", prompt: null },
          },
        };
      });
    });

    es.addEventListener("step-progress", (e) => {
      const d = JSON.parse(e.data);
      setSnapshot((prev) => {
        if (!prev) return prev;
        const existing = prev.stepsByID[d.step_id] ?? { step_id: d.step_id };
        return {
          ...prev,
          stepsByID: {
            ...prev.stepsByID,
            [d.step_id]: { ...existing, progress: d.payload },
          },
        };
      });
    });

    // A step suspended: park it in awaiting_resolution and keep the
    // opaque prompt for the plugin's "resolution" slot.
    es.addEventListener("step-awaiting", (e) => {
      const d = JSON.parse(e.data);
      setSnapshot((prev) => {
        if (!prev) return prev;
        const existing = prev.stepsByID[d.step_id] ?? { step_id: d.step_id };
        return {
          ...prev,
          stepsByID: {
            ...prev.stepsByID,
            [d.step_id]: { ...existing, status: "awaiting_resolution", prompt: d.prompt },
          },
        };
      });
    });

    es.addEventListener("step-finish", (e) => {
      const d = JSON.parse(e.data);
      setSnapshot((prev) => {
        if (!prev) return prev;
        const existing = prev.stepsByID[d.step_id] ?? { step_id: d.step_id };
        return {
          ...prev,
          stepsByID: {
            ...prev.stepsByID,
            [d.step_id]: { ...existing, status: d.status, error: d.error, outputs: d.outputs, prompt: null },
          },
        };
      });
    });

    es.addEventListener("run-status", (e) => {
      const d = JSON.parse(e.data);
      setSnapshot((prev) => (prev ? { ...prev, summary: { ...prev.summary, status: d.status, completed_at: d.completed_at } } : prev));
      loadRuns();
    });

    es.onerror = () => {
      // EventSource auto-reconnects; only surface a message if the
      // browser gives up (readyState === CLOSED).
      if (es.readyState === EventSource.CLOSED) {
        setRunError("Live event stream closed.");
      }
    };

    return () => {
      es.close();
      if (esRef.current === es) esRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedRunID]);

  // POST a run-starting request and select the new run. Throws on
  // failure so the launch modal can show the error.
  const startRun = async (url, body) => {
    const r = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      const txt = await r.text();
      throw new Error(`${r.status}: ${txt}`);
    }
    const { run_id } = await r.json();
    setLaunch(null);
    await loadRuns();
    setSelectedRunID(run_id);
  };

  const runURL = `/api/workflows/${encodeURIComponent(workflow.id)}/run`;

  // A workflow with declared inputs asks for them first; one without
  // starts right away.
  const handleRun = async () => {
    if ((workflow.inputs ?? []).length > 0) {
      setLaunch({ kind: "run" });
      return;
    }
    setTriggering(true);
    setRunError(null);
    try {
      await startRun(runURL, {});
    } catch (e) {
      setRunError(String(e));
    } finally {
      setTriggering(false);
    }
  };

  // Deliver a resolution blob (opaque - produced by the plugin's own
  // "resolution" slot) to a suspended step. The workflow re-executes the
  // step with this blob; SSE then reports it running again.
  const resolveStep = async (stepId, blob) => {
    const r = await fetch(
      `/api/runs/${encodeURIComponent(selectedRunID)}/steps/${encodeURIComponent(stepId)}/resolve`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ resolution: blob ?? null }),
      },
    );
    if (!r.ok) {
      const txt = await r.text();
      throw new Error(`resolve ${stepId} → ${r.status}: ${txt}`);
    }
  };

  const handleCancel = async () => {
    if (!selectedRunID) return;
    if (!confirm(`Cancel run ${selectedRunID}?`)) return;
    try {
      const r = await fetch(`/api/runs/${encodeURIComponent(selectedRunID)}/cancel`, { method: "POST" });
      if (!r.ok) {
        const txt = await r.text();
        throw new Error(`${r.status}: ${txt}`);
      }
    } catch (e) {
      setRunError(String(e));
    }
  };

  const stepsForGraph = useMemo(() => {
    if (!snapshot) return [];
    return (workflow.steps ?? []).map((s) => {
      const rec = snapshot.stepsByID[s.id];
      return {
        step_id: s.id,
        plugin: s.plugin,
        action: s.action,
        status: rec?.status ?? "pending",
        error: rec?.error,
        outputs: rec?.outputs,
        reused_from: rec?.reused_from,
      };
    });
  }, [snapshot, workflow.steps]);

  const selectedSummary = runs?.find((r) => r.run_id === selectedRunID) ?? snapshot?.summary;
  const runFinished = Boolean(selectedSummary) && selectedSummary.status !== "running" && !removed;
  // The rerun plan from a step, or null when it can't be rerun because a
  // step it would reuse didn't succeed in the selected run.
  const rerunFrom = (stepId) => {
    const plan = rerunPlan(workflow.steps ?? [], stepId);
    const ok = plan.reuse.every((id) => {
      const st = snapshot?.stepsByID[id]?.status;
      return st === "succeeded" || st === "reused";
    });
    return ok ? plan : null;
  };
  const runningSteps = (workflow.steps ?? []).filter((s) => snapshot?.stepsByID[s.id]?.status === "running");
  const awaitingSteps = (workflow.steps ?? []).filter((s) => snapshot?.stepsByID[s.id]?.status === "awaiting_resolution");
  const stepDefByID = Object.fromEntries((workflow.steps ?? []).map((s) => [s.id, s]));
  const hasSlot = (manifest, slot) => (manifest?.mfeSlots ?? []).some((x) => x.slot === slot);
  // slotContext shared by every per-run slot (step-runtime, resolution).
  const runSlotContext = (stepDef, rec) => ({
    runId: selectedRunID,
    workflowId: workflow.id,
    stepId: stepDef.id,
    instanceId: stepDef.instance,
    action: stepDef.action,
    status: rec?.status ?? "pending",
    progress: rec?.progress ?? null,
  });
  const openStep = openStepID ? (workflow.steps ?? []).find((s) => s.id === openStepID) : null;
  const openStepPlugin = openStep ? pluginById[openStep.plugin] : null;

  return (
    <Stack hasGutter>
      <StackItem>
        <Split hasGutter style={{ alignItems: "center" }}>
          <SplitItem isFilled>
            <Stack>
              <StackItem>
                <strong style={{ fontSize: 18 }}>
                  {workflow.display_name || workflow.id}
                </strong>
              </StackItem>
              {workflow.schedule && (
                <StackItem>
                  <Label color="blue" isCompact>
                    cron: {workflow.schedule}
                  </Label>
                </StackItem>
              )}
            </Stack>
          </SplitItem>
          <SplitItem>
            {removed ? (
              <Button variant="danger" onClick={() => setPurgeOpen(true)}>
                Purge…
              </Button>
            ) : (
              <Button variant="primary" onClick={handleRun} isDisabled={triggering}>
                {triggering ? <><Spinner size="sm" /> Starting…</> : "Run now"}
              </Button>
            )}
          </SplitItem>
        </Split>
      </StackItem>

      {removed && (
        <StackItem>
          <Alert variant="warning" isInline title="This workflow was removed">
            Its file was no longer in the workflows directory when the
            orchestrator last started (on {formatWhen(workflow.removed_at)}), so
            it can't be run. Its run history and the state plugins keep for it
            are still stored. Restore the file and restart to bring it back, or
            purge it to delete all of that.
          </Alert>
        </StackItem>
      )}

      {runError && (
        <StackItem>
          <Alert variant="danger" title="Run failed" isInline>
            {runError}
          </Alert>
        </StackItem>
      )}
      {runsError && (
        <StackItem>
          <Alert variant="warning" title="Could not load run history" isInline>
            {runsError}
          </Alert>
        </StackItem>
      )}

      <StackItem>
        <Card>
          <CardTitle>Pipeline graph</CardTitle>
          <CardBody style={{ overflowX: "auto" }}>
            <WorkflowGraph
              workflow={workflow}
              run={{ steps: stepsForGraph }}
              onStepClick={(id) => setOpenStepID(id)}
              isStepEditable={(step) => hasSlot(pluginById[step.plugin], "workflow-step-config")}
            />
          </CardBody>
        </Card>
      </StackItem>

      {awaitingSteps.length > 0 && (
        <StackItem>
          <Stack hasGutter>
            {awaitingSteps.map((s) => {
              const rec = snapshot?.stepsByID[s.id];
              const manifest = pluginById[s.plugin];
              return (
                <StackItem key={s.id}>
                  <Card style={{ borderTop: "3px solid #b98412" }}>
                    <CardTitle>
                      Needs input - <code>{s.id}</code> ·{" "}
                      <span style={{ color: "#6a6e73", fontWeight: 400 }}>
                        {s.plugin}/{s.action}
                      </span>
                    </CardTitle>
                    <CardBody>
                      {manifest && hasSlot(manifest, "resolution") ? (
                        <PluginPanel
                          manifest={manifest}
                          mode="resolution"
                          slotContext={{
                            ...runSlotContext(s, rec),
                            prompt: rec?.prompt ?? null,
                            onResolve: (blob) => resolveStep(s.id, blob),
                          }}
                        />
                      ) : (
                        <em style={{ color: "#6a6e73" }}>
                          Plugin {s.plugin} suspended this step but does not provide a
                          resolution panel. Cancel the run to unblock it.
                        </em>
                      )}
                    </CardBody>
                  </Card>
                </StackItem>
              );
            })}
          </Stack>
        </StackItem>
      )}

      {runningSteps.length > 0 && (
        <StackItem>
          <Stack hasGutter>
            {runningSteps.map((s) => {
              const rec = snapshot?.stepsByID[s.id];
              const manifest = pluginById[s.plugin];
              return (
                <StackItem key={s.id}>
                  <Card>
                    <CardTitle>
                      Running - <code>{s.id}</code> ·{" "}
                      <span style={{ color: "#6a6e73", fontWeight: 400 }}>
                        {s.plugin}/{s.action}
                      </span>
                    </CardTitle>
                    <CardBody>
                      {manifest ? (
                        <PluginPanel
                          manifest={manifest}
                          mode="step-runtime"
                          slotContext={runSlotContext(s, rec)}
                        />
                      ) : (
                        <em style={{ color: "#6a6e73" }}>
                          Plugin {s.plugin} not registered - cannot render live progress.
                        </em>
                      )}
                    </CardBody>
                  </Card>
                </StackItem>
              );
            })}
          </Stack>
        </StackItem>
      )}

      <StackItem>
        <Card>
          <CardTitle>Runs</CardTitle>
          <CardBody>
            {runs === null ? (
              <Spinner size="sm" aria-label="Loading runs" />
            ) : runs.length === 0 ? (
              <EmptyState titleText="No runs yet" headingLevel="h4">
                <EmptyStateBody>
                  Trigger the workflow to see run history here.
                </EmptyStateBody>
              </EmptyState>
            ) : (
              <DataList
                aria-label="Runs"
                isCompact
                selectedDataListItemId={selectedRunID}
                onSelectDataListItem={(_e, id) => setSelectedRunID(id)}
              >
                {runs.map((r) => (
                  <DataListItem
                    key={r.run_id}
                    id={r.run_id}
                    aria-labelledby={`run-${r.run_id}`}
                  >
                    <DataListItemRow>
                      <DataListItemCells
                        dataListCells={[
                          <DataListCell key="id" isFilled>
                            <code id={`run-${r.run_id}`} style={{ fontSize: 12 }}>
                              {r.run_id}
                            </code>
                          </DataListCell>,
                          <DataListCell key="status">
                            <Label color={statusColor(r.status)} isCompact>
                              {r.status}
                            </Label>
                          </DataListCell>,
                          <DataListCell key="started">
                            <div style={{ fontSize: 12, color: "#6a6e73" }}>
                              {formatWhen(r.started_at)}
                            </div>
                          </DataListCell>,
                          <DataListCell key="dur">
                            <div style={{ fontSize: 12, color: "#6a6e73" }}>
                              {formatDuration(r.started_at, r.completed_at)}
                            </div>
                          </DataListCell>,
                        ]}
                      />
                    </DataListItemRow>
                  </DataListItem>
                ))}
              </DataList>
            )}
          </CardBody>
        </Card>
      </StackItem>

      {selectedSummary && (
        <StackItem>
          <Card>
            <CardTitle>
              <Split hasGutter style={{ alignItems: "center" }}>
                <SplitItem isFilled>
                  Run - <code style={{ fontSize: 12 }}>{selectedSummary.run_id}</code>
                </SplitItem>
                <SplitItem>
                  <Label color={statusColor(selectedSummary.status)}>{selectedSummary.status}</Label>
                </SplitItem>
                {selectedSummary.status === "running" && (
                  <SplitItem>
                    <Button variant="danger" size="sm" onClick={handleCancel}>Cancel</Button>
                  </SplitItem>
                )}
              </Split>
            </CardTitle>
            <CardBody>
              <RunInfo
                summary={selectedSummary}
                onSelectRun={runs?.some((r) => r.run_id === selectedSummary.rerun_of) ? setSelectedRunID : null}
              />
              {stepsForGraph.length === 0 ? (
                <p style={{ color: "#6a6e73", margin: 0 }}>
                  {selectedSummary.status === "running"
                    ? "Waiting for the first step to start…"
                    : "No step records for this run."}
                </p>
              ) : (
                <DescriptionList isCompact>
                  {stepsForGraph.map((s) => (
                    <DescriptionListGroup key={s.step_id}>
                      <DescriptionListTerm>
                        <Split hasGutter style={{ alignItems: "center" }}>
                          <SplitItem>{s.step_id}</SplitItem>
                          <SplitItem>
                            <Label color={stepColor(s.status)} isCompact>{s.status}</Label>
                          </SplitItem>
                          {runFinished && (() => {
                            const plan = rerunFrom(s.step_id);
                            return (
                              <SplitItem>
                                <Button
                                  variant="link"
                                  isInline
                                  size="sm"
                                  isDisabled={!plan}
                                  title={plan ? undefined : "A step this would reuse did not succeed in this run"}
                                  onClick={() => setLaunch({
                                    kind: "rerun",
                                    fromStep: s.step_id,
                                    plan,
                                    sourceRunID: selectedSummary.run_id,
                                    sourceInputs: selectedSummary.inputs ?? {},
                                  })}
                                >
                                  Rerun from here
                                </Button>
                              </SplitItem>
                            );
                          })()}
                        </Split>
                      </DescriptionListTerm>
                      <DescriptionListDescription>
                        {s.reused_from && (
                          <div style={{ fontSize: 12, color: "#6a6e73" }}>
                            Not executed; outputs from <code>{s.reused_from}</code>
                          </div>
                        )}
                        {s.error && (
                          <Alert variant="danger" isInline isPlain title="Error">
                            <code style={{ whiteSpace: "pre-wrap" }}>{s.error}</code>
                          </Alert>
                        )}
                        {s.status !== "running" && s.status !== "pending" && s.status !== "reused" &&
                          hasSlot(pluginById[s.plugin], "step-runtime") && stepDefByID[s.step_id] && (
                          <ExpandableSection toggleText="Details">
                            <PluginPanel
                              manifest={pluginById[s.plugin]}
                              mode="step-runtime"
                              slotContext={runSlotContext(stepDefByID[s.step_id], snapshot?.stepsByID[s.step_id])}
                            />
                          </ExpandableSection>
                        )}
                        {s.outputs && Object.keys(s.outputs).length > 0 && (
                          <ExpandableSection toggleText="Outputs">
                            <pre style={{ margin: 0, fontSize: 12, whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                              {JSON.stringify(s.outputs, null, 2)}
                            </pre>
                          </ExpandableSection>
                        )}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  ))}
                </DescriptionList>
              )}
            </CardBody>
          </Card>
        </StackItem>
      )}

      <Modal
        variant={ModalVariant.large}
        isOpen={openStep != null}
        onClose={() => setOpenStepID(null)}
        aria-label="Step configuration"
      >
        <ModalHeader
          title={openStep ? openStep.id : ""}
          description={
            openStep ? (
              <span style={{ color: "#6a6e73", fontSize: 12 }}>
                <code>{openStep.plugin}</code> / <code>{openStep.action}</code>
              </span>
            ) : null
          }
        />
        <ModalBody>
          {openStep && openStepPlugin ? (
            (openStepPlugin.mfeSlots ?? []).some((s) => s.slot === "workflow-step-config") ? (
              <PluginPanel
                manifest={openStepPlugin}
                mode="workflow-step-config"
                slotContext={{
                  workflowId: workflow.id,
                  stepId: openStep.id,
                  instanceId: openStep.instance,
                  action: openStep.action,
                  params: openStep.params,
                }}
              />
            ) : (
              <EmptyState titleText="No options here" headingLevel="h4">
                <EmptyStateBody>
                  This plugin does not expose per-workflow options for this step.
                </EmptyStateBody>
              </EmptyState>
            )
          ) : null}
        </ModalBody>
      </Modal>

      {launch?.kind === "run" && (
        <RunLaunchModal
          workflow={workflow}
          title={`Run ${workflow.display_name || workflow.id}`}
          startValues={initialInputs(workflow.inputs, null)}
          onSubmit={(inputs) => startRun(runURL, { inputs })}
          onClose={() => setLaunch(null)}
        />
      )}
      {launch?.kind === "rerun" && (
        <RunLaunchModal
          workflow={workflow}
          title={`Rerun from ${launch.fromStep}`}
          plan={launch.plan}
          sourceRunID={launch.sourceRunID}
          startValues={initialInputs(workflow.inputs, launch.sourceInputs)}
          onSubmit={(inputs) => startRun(
            `/api/runs/${encodeURIComponent(launch.sourceRunID)}/rerun`,
            { from_step: launch.fromStep, inputs },
          )}
          onClose={() => setLaunch(null)}
        />
      )}

      {purgeOpen && (
        <PurgeModal
          workflow={workflow}
          runCount={runs?.length ?? 0}
          onClose={() => setPurgeOpen(false)}
          onPurged={() => {
            setPurgeOpen(false);
            onPurged?.(workflow.id);
          }}
        />
      )}
    </Stack>
  );
}

// PurgeModal confirms and runs POST /api/workflows/:id/purge. The user
// types the workflow ID to confirm, since nothing can bring the data back.
function PurgeModal({ workflow, runCount, onClose, onPurged }) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  const purge = async () => {
    setBusy(true);
    setError(null);
    try {
      const r = await fetch(`/api/workflows/${encodeURIComponent(workflow.id)}/purge`, { method: "POST" });
      if (r.ok) {
        onPurged();
        return;
      }
      // 502 carries a JSON report naming the plugins that failed;
      // everything else is a plain-text reason.
      const txt = await r.text();
      let msg = txt;
      try {
        const body = JSON.parse(txt);
        const perPlugin = Object.entries(body.report?.plugin_errors ?? {})
          .map(([id, e]) => `${id}: ${e}`);
        msg = [body.error, ...perPlugin].filter(Boolean).join("\n");
      } catch {
        // not JSON
      }
      throw new Error(`${r.status}: ${msg}`);
    } catch (e) {
      setError(String(e.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal variant={ModalVariant.small} isOpen onClose={busy ? undefined : onClose} aria-label="Purge workflow">
      <ModalHeader title={`Purge ${workflow.display_name || workflow.id}?`} titleIconVariant="danger" />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            This permanently deletes:
            <ul style={{ margin: "8px 0 0 20px" }}>
              <li>the workflow's definition and its {runCount}{runCount >= 50 ? "+" : ""} recorded run(s)</li>
              <li>the runs' history in Temporal</li>
              <li>
                everything plugins keep for it: patch queues, saved conflict
                resolutions and decisions, rerere cache, command history
              </li>
            </ul>
          </StackItem>
          <StackItem>
            Type <code>{workflow.id}</code> to confirm.
          </StackItem>
          <StackItem>
            <TextInput
              aria-label="Workflow ID"
              value={typed}
              onChange={(_e, v) => setTyped(v)}
              isDisabled={busy}
              autoFocus
            />
          </StackItem>
          {error && (
            <StackItem>
              <Alert variant="danger" isInline title="Purge failed">
                <pre style={{ margin: 0, whiteSpace: "pre-wrap", fontSize: 12 }}>{error}</pre>
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="danger" onClick={purge} isDisabled={busy || typed !== workflow.id} isLoading={busy}>
          Purge
        </Button>
        <Button variant="link" onClick={onClose} isDisabled={busy}>
          Cancel
        </Button>
      </ModalFooter>
    </Modal>
  );
}

function statusColor(s) {
  if (s === "succeeded") return "green";
  if (s === "failed") return "red";
  if (s === "running") return "blue";
  return "grey";
}

// RunInfo shows a run's inputs and, for a rerun, where it came from.
// onSelectRun, when set, makes the source run a link.
export function RunInfo({ summary, onSelectRun }) {
  const inputs = Object.entries(summary.inputs ?? {});
  if (!summary.rerun_of && inputs.length === 0) return null;
  return (
    <DescriptionList isCompact isHorizontal style={{ marginBottom: 16 }}>
      {summary.rerun_of && (
        <DescriptionListGroup>
          <DescriptionListTerm>Rerun of</DescriptionListTerm>
          <DescriptionListDescription>
            {onSelectRun ? (
              <Button variant="link" isInline onClick={() => onSelectRun(summary.rerun_of)}>
                <code style={{ fontSize: 12 }}>{summary.rerun_of}</code>
              </Button>
            ) : (
              <code style={{ fontSize: 12 }}>{summary.rerun_of}</code>
            )}{" "}
            from step <code>{summary.rerun_from_step}</code>
          </DescriptionListDescription>
        </DescriptionListGroup>
      )}
      {inputs.length > 0 && (
        <DescriptionListGroup>
          <DescriptionListTerm>Inputs</DescriptionListTerm>
          <DescriptionListDescription>
            {inputs.map(([k, v]) => (
              <div key={k}>
                <code>{k}</code> = <code>{v === "" ? '""' : v}</code>
              </div>
            ))}
          </DescriptionListDescription>
        </DescriptionListGroup>
      )}
    </DescriptionList>
  );
}

function stepColor(s) {
  if (s === "succeeded") return "green";
  if (s === "reused") return "teal";
  if (s === "failed") return "red";
  if (s === "skipped") return "orange";
  if (s === "cancelled") return "grey";
  if (s === "running") return "blue";
  if (s === "awaiting_resolution") return "yellow";
  return "grey";
}

function formatWhen(ts) {
  if (!ts) return "-";
  const d = new Date(ts);
  return d.toLocaleString();
}

function formatDuration(startTs, endTs) {
  if (!startTs) return "-";
  const start = new Date(startTs).getTime();
  const end = endTs ? new Date(endTs).getTime() : Date.now();
  const ms = Math.max(0, end - start);
  if (ms < 1000) return `${ms}ms`;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rs = s % 60;
  if (m < 60) return `${m}m ${rs}s`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return `${h}h ${rm}m`;
}
