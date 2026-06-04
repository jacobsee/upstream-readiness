import { useEffect, useRef, useState } from "react";
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
  Spinner,
  Split,
  SplitItem,
  Stack,
  StackItem,
  ToggleGroup,
  ToggleGroupItem,
} from "@patternfly/react-core";

// Cross-workflow "Runs" dashboard. Lists recent runs across every
// workflow, with a status filter. Selecting a run opens its step-level
// detail alongside. Runs in flight are polled until they finish.
export default function RunsView({ workflows, onOpenWorkflow }) {
  const [runs, setRuns] = useState(null);
  const [error, setError] = useState(null);
  const [statusFilter, setStatusFilter] = useState("all");
  const [selectedRunID, setSelectedRunID] = useState(null);
  const [selectedRun, setSelectedRun] = useState(null);
  const [detailError, setDetailError] = useState(null);
  const listTimer = useRef(null);
  const detailTimer = useRef(null);

  const workflowName = (id) => {
    const wf = workflows?.find((w) => w.id === id);
    return wf?.display_name || id;
  };
  const isRemoved = (id) => Boolean(workflows?.find((w) => w.id === id)?.removed_at);

  const loadRuns = async () => {
    try {
      const q = statusFilter === "all" ? "" : `?status=${encodeURIComponent(statusFilter)}`;
      const r = await fetch(`/api/runs${q}`);
      if (!r.ok) throw new Error(`/api/runs → ${r.status}`);
      setRuns((await r.json()) ?? []);
    } catch (e) {
      setError(String(e));
    }
  };

  // Refresh the list on mount + when the status filter changes, and
  // every 3s while any listed run is still running so the row updates.
  useEffect(() => {
    setRuns(null);
    setError(null);
    loadRuns();
    return () => {
      if (listTimer.current) clearTimeout(listTimer.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [statusFilter]);

  useEffect(() => {
    if (listTimer.current) clearTimeout(listTimer.current);
    if (!runs) return;
    if (runs.some((r) => r.status === "running")) {
      listTimer.current = setTimeout(loadRuns, 3000);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runs]);

  // Poll the selected run's detail while it's running.
  useEffect(() => {
    if (detailTimer.current) clearTimeout(detailTimer.current);
    setSelectedRun(null);
    setDetailError(null);
    if (!selectedRunID) return;
    let cancelled = false;

    const pollOnce = async () => {
      try {
        const r = await fetch(`/api/runs/${encodeURIComponent(selectedRunID)}`);
        if (!r.ok) throw new Error(`/api/runs → ${r.status}`);
        const summary = await r.json();
        if (cancelled) return;
        setSelectedRun(summary);
        if (summary.status === "running") {
          detailTimer.current = setTimeout(pollOnce, 1500);
        }
      } catch (e) {
        if (!cancelled) setDetailError(String(e));
      }
    };
    pollOnce();

    return () => {
      cancelled = true;
      if (detailTimer.current) clearTimeout(detailTimer.current);
    };
  }, [selectedRunID]);

  const steps = parseSteps(selectedRun);

  return (
    <Stack hasGutter>
      <StackItem>
        <Split hasGutter style={{ alignItems: "center" }}>
          <SplitItem isFilled>
            <strong style={{ fontSize: 18 }}>Runs</strong>
          </SplitItem>
          <SplitItem>
            <ToggleGroup aria-label="Status filter">
              {["all", "running", "succeeded", "failed"].map((s) => (
                <ToggleGroupItem
                  key={s}
                  text={s}
                  buttonId={`status-${s}`}
                  isSelected={statusFilter === s}
                  onChange={() => setStatusFilter(s)}
                />
              ))}
            </ToggleGroup>
          </SplitItem>
        </Split>
      </StackItem>

      {error && (
        <StackItem>
          <Alert variant="warning" title="Could not load runs" isInline>
            {error}
          </Alert>
        </StackItem>
      )}

      <StackItem>
        <Card>
          <CardTitle>Recent runs</CardTitle>
          <CardBody>
            {runs === null ? (
              <Spinner size="sm" aria-label="Loading runs" />
            ) : runs.length === 0 ? (
              <EmptyState titleText="No runs" headingLevel="h4">
                <EmptyStateBody>
                  Nothing matches the current filter.
                </EmptyStateBody>
              </EmptyState>
            ) : (
              <DataList
                aria-label="All runs"
                isCompact
                selectedDataListItemId={selectedRunID}
                onSelectDataListItem={(_e, id) => setSelectedRunID(id)}
              >
                {runs.map((r) => (
                  <DataListItem key={r.run_id} id={r.run_id} aria-labelledby={`allrun-${r.run_id}`}>
                    <DataListItemRow>
                      <DataListItemCells
                        dataListCells={[
                          <DataListCell key="workflow" isFilled>
                            <button
                              id={`allrun-${r.run_id}`}
                              onClick={(e) => {
                                e.stopPropagation();
                                onOpenWorkflow?.(r.workflow_id);
                              }}
                              style={{
                                background: "none",
                                border: "none",
                                padding: 0,
                                color: "#06c",
                                cursor: "pointer",
                                fontWeight: 600,
                              }}
                            >
                              {workflowName(r.workflow_id)}
                            </button>
                            {isRemoved(r.workflow_id) && (
                              <Label color="grey" isCompact style={{ marginLeft: 6 }}>
                                removed
                              </Label>
                            )}
                            <div style={{ fontSize: 12, color: "#6a6e73" }}>
                              <code>{r.run_id}</code>
                            </div>
                          </DataListCell>,
                          <DataListCell key="status">
                            <Label color={statusColor(r.status)} isCompact>{r.status}</Label>
                            {r.awaiting_steps > 0 && (
                              <Label color="yellow" isCompact style={{ marginLeft: 4 }}>
                                needs input
                              </Label>
                            )}
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

      {detailError && (
        <StackItem>
          <Alert variant="danger" title="Could not load run" isInline>
            {detailError}
          </Alert>
        </StackItem>
      )}

      {selectedRun && (
        <StackItem>
          <Card>
            <CardTitle>
              <Split hasGutter style={{ alignItems: "center" }}>
                <SplitItem isFilled>
                  <code style={{ fontSize: 12 }}>{selectedRun.run_id}</code>
                </SplitItem>
                <SplitItem>
                  <Label color={statusColor(selectedRun.status)}>{selectedRun.status}</Label>
                </SplitItem>
                {selectedRun.status === "running" && (
                  <SplitItem>
                    <Button
                      variant="danger"
                      size="sm"
                      onClick={async () => {
                        if (!confirm(`Cancel run ${selectedRun.run_id}?`)) return;
                        try {
                          const r = await fetch(`/api/runs/${encodeURIComponent(selectedRun.run_id)}/cancel`, { method: "POST" });
                          if (!r.ok) {
                            const txt = await r.text();
                            throw new Error(`${r.status}: ${txt}`);
                          }
                        } catch (e) {
                          setDetailError(String(e));
                        }
                      }}
                    >
                      Cancel
                    </Button>
                  </SplitItem>
                )}
              </Split>
            </CardTitle>
            <CardBody>
              {steps.length === 0 ? (
                <p style={{ color: "#6a6e73", margin: 0 }}>
                  {selectedRun.status === "running"
                    ? "Waiting for the first step to start…"
                    : "No step records for this run."}
                </p>
              ) : (
                <DescriptionList isCompact>
                  {steps.map((s) => (
                    <DescriptionListGroup key={s.step_id}>
                      <DescriptionListTerm>
                        <Split hasGutter style={{ alignItems: "center" }}>
                          <SplitItem>{s.step_id}</SplitItem>
                          <SplitItem>
                            <Label color={stepColor(s.status)} isCompact>{s.status}</Label>
                          </SplitItem>
                        </Split>
                      </DescriptionListTerm>
                      <DescriptionListDescription>
                        {s.error && (
                          <Alert variant="danger" isInline isPlain title="Error">
                            <code style={{ whiteSpace: "pre-wrap" }}>{s.error}</code>
                          </Alert>
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
    </Stack>
  );
}

function parseSteps(summary) {
  if (!summary || !summary.results) return [];
  const res = typeof summary.results === "string" ? JSON.parse(summary.results) : summary.results;
  return res.steps ?? [];
}

function statusColor(s) {
  if (s === "succeeded") return "green";
  if (s === "failed") return "red";
  if (s === "running") return "blue";
  return "grey";
}

function stepColor(s) {
  if (s === "succeeded") return "green";
  if (s === "failed") return "red";
  if (s === "skipped") return "orange";
  if (s === "running") return "blue";
  if (s === "awaiting_resolution") return "yellow";
  return "grey";
}

function formatWhen(ts) {
  if (!ts) return "-";
  return new Date(ts).toLocaleString();
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
