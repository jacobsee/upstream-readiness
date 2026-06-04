// upstream-rebase MFE. One component, mounted by the orchestrator shell
// in several slots via the `mode` prop:
//
//   mode="instance-config"       create/edit an instance (shared form)
//   mode="instance-list-item"    one-line instance summary (shared)
//   mode="step-runtime"          live progress + the commit table
//   mode="resolution"            answer a suspended rebase: carry/drop
//                                decisions, or resolve a conflict
//   mode="workflow-step-config"  per-step patch queue + saved
//                                resolutions / decisions library
//
// All state lives in the plugin's own Postgres and is reached only via
// pluginApi.execute (run_get, conflict_get, resolution_save, …).

import { useEffect, useMemo, useState } from "react";
import {
  ActionGroup,
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Checkbox,
  Content,
  ExpandableSection,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  Label,
  Radio,
  Split,
  SplitItem,
  Stack,
  StackItem,
  Tab,
  Tabs,
  TabTitleText,
  TextInput,
} from "@patternfly/react-core";
import { LogTail } from "@pluginkit/fields.jsx";
import { GitInstanceConfigForm, GitInstanceSummary } from "@pluginkit/GitInstanceConfigForm.jsx";
import { PatchQueueEditor } from "@pluginkit/PatchQueueEditor.jsx";
import { CommandHistory } from "@pluginkit/CommandHistory.jsx";
import { CodeEditor } from "./CodeEditor.jsx";

export default function RebasePanel(props) {
  const mode = props.mode;
  if (mode === "instance-config") {
    return <GitInstanceConfigForm {...props} newTitle="New Upstream Rebase instance" />;
  }
  if (mode === "instance-list-item") return <GitInstanceSummary {...props} />;
  if (mode === "step-runtime") return <StepRuntimePanel {...props} />;
  if (mode === "resolution") return <ResolutionPanel {...props} />;
  if (mode === "workflow-step-config") return <StepConfigPanel {...props} />;
  return (
    <em style={{ color: "#6a6e73" }}>
      Upstream Rebase runs only as a workflow step. Add a step with{" "}
      <code>plugin: upstream-rebase</code> and <code>action: rebase</code>.
    </em>
  );
}

// ── shared bits ───────────────────────────────────────────────────────────────

const STATUS_COLOR = {
  pending: "grey",
  picked: "green",
  resolved: "teal",
  already_upstream: "purple",
  dropped: "grey",
  requeued_patch: "blue",
  kept_empty: "grey",
  skipped: "orange",
  acknowledged: "orange",
  conflict: "red",
  awaiting_decision: "yellow",
};

const STATUS_HELP = {
  pending: "not processed yet",
  picked: "cherry-picked cleanly",
  resolved: "picked after resolving a conflict",
  already_upstream: "upstream PR is already in the target; pick was empty",
  dropped: "not carried (UPSTREAM: <drop>, or a drop decision)",
  requeued_patch: "came from this step's patch queue; re-applied from the queue, not picked",
  kept_empty: "became empty; kept as an empty commit by decision",
  skipped: "skipped by a conflict resolution",
  acknowledged: "merge-only content acknowledged as not carried",
  conflict: "waiting for a conflict resolution",
  awaiting_decision: "waiting for a carry/drop decision",
};

const KIND_LABEL = {
  carry: "carry",
  drop: "drop",
  pr: "PR",
  malformed: "malformed",
  unclassified: "no prefix",
  evil_merge: "merge content",
  queued_patch: "patch queue",
};

function short(sha) {
  return sha ? sha.slice(0, 12) : "";
}

// PR links only when the upstream is on GitHub.
function prURL(upstreamURL, n) {
  if (!upstreamURL || !n) return null;
  const m = upstreamURL.match(/github\.com[/:]([^/]+)\/([^/.]+)/);
  return m ? `https://github.com/${m[1]}/${m[2]}/pull/${n}` : null;
}

function StatusLabel({ status }) {
  return (
    <span title={STATUS_HELP[status] ?? status}>
      <Label isCompact color={STATUS_COLOR[status] ?? "grey"}>{status}</Label>
    </span>
  );
}

function useRun(pluginApi, slotContext, rev) {
  const { runId, workflowId, stepId, instanceId } = slotContext ?? {};
  const [data, setData] = useState(null);
  const [error, setError] = useState(null);
  const reload = async () => {
    if (!runId || !instanceId) return;
    try {
      const res = await pluginApi.execute("run_get", {}, { instanceId, workflowId, stepId, runId });
      setData(res);
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  };
  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId, stepId, instanceId, rev]);
  return { data, error, reload };
}

function Counts({ counts, total }) {
  const entries = Object.entries(counts ?? {}).sort((a, b) => b[1] - a[1]);
  if (entries.length === 0) return null;
  return (
    <Split hasGutter style={{ flexWrap: "wrap", alignItems: "center" }}>
      <SplitItem><strong>{total}</strong> commits</SplitItem>
      {entries.map(([st, n]) => (
        <SplitItem key={st}>
          <StatusLabel status={st} /> {n}
        </SplitItem>
      ))}
    </Split>
  );
}

// CommitTable: every commit the rebase had to consider, in pick order.
function CommitTable({ commits, upstreamURL, highlight }) {
  const [statusFilter, setStatusFilter] = useState("all");
  const [kindFilter, setKindFilter] = useState("all");
  const [q, setQ] = useState("");
  const statuses = useMemo(() => [...new Set((commits ?? []).map((c) => c.status))].sort(), [commits]);
  const kinds = useMemo(() => [...new Set((commits ?? []).map((c) => c.kind))].sort(), [commits]);
  const rows = (commits ?? []).filter(
    (c) =>
      (statusFilter === "all" || c.status === statusFilter) &&
      (kindFilter === "all" || c.kind === kindFilter) &&
      (!q || c.subject.toLowerCase().includes(q.toLowerCase()) || c.orig_sha.startsWith(q)),
  );
  return (
    <Stack hasGutter>
      <StackItem>
        <Split hasGutter style={{ flexWrap: "wrap" }}>
          <SplitItem>
            <FormSelect value={statusFilter} onChange={(_, v) => setStatusFilter(v)} aria-label="Status filter">
              <FormSelectOption value="all" label="All statuses" />
              {statuses.map((s) => <FormSelectOption key={s} value={s} label={s} />)}
            </FormSelect>
          </SplitItem>
          <SplitItem>
            <FormSelect value={kindFilter} onChange={(_, v) => setKindFilter(v)} aria-label="Kind filter">
              <FormSelectOption value="all" label="All kinds" />
              {kinds.map((k) => <FormSelectOption key={k} value={k} label={KIND_LABEL[k] ?? k} />)}
            </FormSelect>
          </SplitItem>
          <SplitItem isFilled>
            <TextInput value={q} onChange={(_, v) => setQ(v)} placeholder="Filter by subject or SHA" aria-label="Search" />
          </SplitItem>
          <SplitItem style={{ alignSelf: "center", color: "#6a6e73", fontSize: 12 }}>
            {rows.length} of {(commits ?? []).length}
          </SplitItem>
        </Split>
      </StackItem>
      <StackItem>
        <div style={{ overflowX: "auto", maxHeight: 520, overflowY: "auto" }}>
          <table className="pf-v6-c-table pf-m-compact" role="grid" aria-label="Commits">
            <thead className="pf-v6-c-table__thead">
              <tr className="pf-v6-c-table__tr">
                <th className="pf-v6-c-table__th">#</th>
                <th className="pf-v6-c-table__th">Original</th>
                <th className="pf-v6-c-table__th">Kind</th>
                <th className="pf-v6-c-table__th">Subject</th>
                <th className="pf-v6-c-table__th">Status</th>
                <th className="pf-v6-c-table__th">New</th>
                <th className="pf-v6-c-table__th">Notes</th>
              </tr>
            </thead>
            <tbody className="pf-v6-c-table__tbody">
              {rows.map((c) => {
                const link = c.kind === "pr" ? prURL(upstreamURL, c.pr) : null;
                return (
                  <tr
                    key={c.ordinal}
                    className="pf-v6-c-table__tr"
                    style={c.ordinal === highlight ? { background: "#fdf7e7" } : undefined}
                  >
                    <td className="pf-v6-c-table__td">{c.ordinal}</td>
                    <td className="pf-v6-c-table__td"><code title={c.orig_sha}>{short(c.orig_sha)}</code></td>
                    <td className="pf-v6-c-table__td">
                      {c.kind === "pr" ? (
                        link ? <a href={link} target="_blank" rel="noreferrer">PR #{c.pr}</a> : `PR #${c.pr}`
                      ) : (
                        KIND_LABEL[c.kind] ?? c.kind
                      )}
                    </td>
                    <td className="pf-v6-c-table__td">
                      <div>{c.subject}</div>
                      <div style={{ fontSize: 11, color: "#6a6e73" }}>{c.author}</div>
                    </td>
                    <td className="pf-v6-c-table__td"><StatusLabel status={c.status} /></td>
                    <td className="pf-v6-c-table__td"><code title={c.new_sha}>{short(c.new_sha)}</code></td>
                    <td className="pf-v6-c-table__td" style={{ fontSize: 12 }}>
                      {c.resolution_source && <Label isCompact color="teal">{c.resolution_source}</Label>}{" "}
                      {c.note}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </StackItem>
    </Stack>
  );
}

// ── step-runtime ──────────────────────────────────────────────────────────────
// Mounted while running and (from the run details) after the step ends.
// The progress payload is small ({phase, message, log, counts, current,
// table_rev}); the table itself is re-fetched via run_get whenever
// table_rev changes, so updates are push-driven over the shell's SSE.

function StepRuntimePanel({ pluginApi, slotContext }) {
  const p = slotContext?.progress;
  const rev = p?.table_rev ?? slotContext?.status;
  const { data, error } = useRun(pluginApi, slotContext, rev);
  const [historyOpen, setHistoryOpen] = useState(false);
  const run = data?.run;
  return (
    <Stack hasGutter>
      {p && (
        <StackItem>
          <Split hasGutter style={{ alignItems: "center" }}>
            <SplitItem><Label color="blue" isCompact>{p.phase || "running"}</Label></SplitItem>
            <SplitItem isFilled>
              <code style={{ fontSize: 12 }}>
                {p.current ? `[${p.current.ordinal}/${p.total}] ${p.current.subject}` : p.message}
              </code>
            </SplitItem>
          </Split>
        </StackItem>
      )}
      {run && (
        <StackItem>
          <Content component="small">
            <code>{run.fork_branch}</code> @ <code>{short(run.fork_tip)}</code> → onto{" "}
            <code>{run.target_ref}</code>. Previous rebase <code>{short(run.prev_merge)}</code>
            {run.prev_base_tag ? <> on <code>{run.prev_base_tag}</code></> : null}.
            {run.pushed_sha && <> Pushed <code>{run.branch}</code> @ <code>{short(run.pushed_sha)}</code>.</>}
          </Content>
        </StackItem>
      )}
      {run?.error && (
        <StackItem>
          <Alert variant="danger" isInline title="Rebase failed">
            <pre style={{ margin: 0, fontSize: 12, whiteSpace: "pre-wrap" }}>{run.error}</pre>
          </Alert>
        </StackItem>
      )}
      {error && (
        <StackItem>
          <Alert variant="warning" isInline title="Could not load the commit table">{error}</Alert>
        </StackItem>
      )}
      {(p?.counts || data?.commits?.length > 0) && (
        <StackItem>
          <Counts counts={p?.counts ?? countBy(data?.commits)} total={p?.total ?? data?.commits?.length} />
        </StackItem>
      )}
      {data?.commits?.length > 0 && (
        <StackItem>
          <CommitTable commits={data.commits} upstreamURL={run?.upstream_url} highlight={p?.current?.ordinal} />
        </StackItem>
      )}
      {run?.range_diff && (
        <StackItem>
          <ExpandableSection toggleText="Range diff (previous carries → rebased carries)">
            <LogTail log={run.range_diff} maxHeight={500} />
          </ExpandableSection>
        </StackItem>
      )}
      <StackItem>
        <ExpandableSection
          toggleText="Command history (every command and its output, all executions)"
          isExpanded={historyOpen}
          onToggle={(_, v) => setHistoryOpen(v)}
        >
          {/* Mounted only when open so a collapsed section costs nothing. */}
          {historyOpen && (
            <CommandHistory
              pluginApi={pluginApi}
              scope={slotContext}
              liveKey={slotContext?.status === "running" ? p : undefined}
            />
          )}
        </ExpandableSection>
      </StackItem>
    </Stack>
  );
}

function countBy(commits) {
  const m = {};
  (commits ?? []).forEach((c) => {
    m[c.status] = (m[c.status] ?? 0) + 1;
  });
  return m;
}

// ── resolution ────────────────────────────────────────────────────────────────
// slotContext.prompt is what the plugin suspended with:
//   { kind: "decide", pending }                       → DecisionForm
//   { kind: "conflict", conflict_id, ordinal, subject } → ConflictForm
// Both save their answer through plugin actions, then call
// slotContext.onResolve({action: "continue"}) so the orchestrator
// re-executes the step. "Abort" resolves with {action: "abort"}.

function ResolutionPanel({ pluginApi, slotContext }) {
  const prompt = slotContext?.prompt;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [resolvedBy, setResolvedBy] = useState("");
  const [abortReason, setAbortReason] = useState("");
  const { data, reload } = useRun(pluginApi, slotContext, prompt?.conflict_id ?? prompt?.pending);

  const finish = async (save) => {
    setBusy(true);
    setError(null);
    try {
      if (save) await save();
      await slotContext.onResolve({ action: "continue" });
    } catch (e) {
      setError(String(e));
      reload();
    } finally {
      setBusy(false);
    }
  };

  const abort = async () => {
    setBusy(true);
    setError(null);
    try {
      await slotContext.onResolve({ action: "abort", reason: abortReason });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const who = resolvedBy.trim() ? `human:${resolvedBy.trim()}` : "human";

  return (
    <Stack hasGutter>
      {error && (
        <StackItem>
          <Alert variant="danger" isInline title="Could not save">
            <pre style={{ margin: 0, fontSize: 12, whiteSpace: "pre-wrap" }}>{error}</pre>
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <FormGroup label="Your name (recorded with the answer)" fieldId="resolved-by">
          <TextInput id="resolved-by" value={resolvedBy} onChange={(_, v) => setResolvedBy(v)} placeholder="optional" />
        </FormGroup>
      </StackItem>
      <StackItem>
        {prompt?.kind === "conflict" ? (
          <ConflictForm pluginApi={pluginApi} slotContext={slotContext} conflictId={prompt.conflict_id}
            busy={busy} who={who} onDone={finish} />
        ) : (
          <DecisionForm pluginApi={pluginApi} slotContext={slotContext} pending={data?.pending ?? []}
            busy={busy} who={who} onDone={finish} />
        )}
      </StackItem>
      {data?.commits?.length > 0 && (
        <StackItem>
          <ExpandableSection toggleText={`All commits (${data.commits.length})`}>
            <Counts counts={countBy(data.commits)} total={data.commits.length} />
            <CommitTable commits={data.commits} upstreamURL={data.run?.upstream_url} highlight={prompt?.ordinal} />
          </ExpandableSection>
        </StackItem>
      )}
      <StackItem>
        <ExpandableSection toggleText="Abort the rebase">
          <Split hasGutter>
            <SplitItem isFilled>
              <TextInput value={abortReason} onChange={(_, v) => setAbortReason(v)} placeholder="Why (optional)" aria-label="Abort reason" />
            </SplitItem>
            <SplitItem>
              <Button variant="danger" onClick={abort} isDisabled={busy}>Abort - fail this step</Button>
            </SplitItem>
          </Split>
        </ExpandableSection>
      </StackItem>
    </Stack>
  );
}

const QUESTION_OPTIONS = {
  classify: [
    { value: "carry", label: "Carry - cherry-pick it like an UPSTREAM: <carry>" },
    { value: "drop", label: "Drop - do not carry it" },
  ],
  empty: [
    { value: "drop", label: "Drop - the target already contains this change" },
    { value: "keep_empty", label: "Keep as an empty commit (preserve the message)" },
  ],
};

function DecisionForm({ pluginApi, slotContext, pending, busy, who, onDone }) {
  const { workflowId, stepId, instanceId, runId } = slotContext ?? {};
  const [answers, setAnswers] = useState({});
  useEffect(() => {
    const init = {};
    pending.forEach((p) => {
      init[p.orig_sha] = { decision: p.saved?.decision ?? "", reason: p.saved?.reason ?? "" };
    });
    setAnswers(init);
  }, [pending.map((p) => p.orig_sha).join(",")]); // eslint-disable-line react-hooks/exhaustive-deps

  const set = (sha, k, v) => setAnswers((a) => ({ ...a, [sha]: { ...a[sha], [k]: v } }));
  const complete = pending.length > 0 && pending.every((p) => answers[p.orig_sha]?.decision && answers[p.orig_sha]?.reason?.trim());

  const save = () =>
    pluginApi.execute(
      "decisions_save",
      {
        decided_by: who,
        decisions: pending.map((p) => ({
          orig_sha: p.orig_sha,
          question: p.question,
          decision: answers[p.orig_sha].decision,
          reason: answers[p.orig_sha].reason,
          subject: p.subject,
        })),
      },
      { instanceId, workflowId, stepId, runId },
    );

  if (pending.length === 0) {
    return (
      <Stack hasGutter>
        <StackItem><em>No commits are waiting for a decision. Continue to let the rebase re-check.</em></StackItem>
        <StackItem><Button onClick={() => onDone(null)} isDisabled={busy}>Continue</Button></StackItem>
      </Stack>
    );
  }

  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          {pending.length} commit(s) can't be handled mechanically. Every decision is saved and reused on
          later runs of this step (a <em>carry</em> also applies to later commits with the same subject;
          a <em>drop</em> must be re-confirmed for each new commit).
        </Content>
      </StackItem>
      {pending.map((p) => (
        <StackItem key={p.orig_sha}>
          <Card isCompact>
            <CardTitle>
              <Split hasGutter style={{ alignItems: "center" }}>
                <SplitItem><Label isCompact color="yellow">{KIND_LABEL[p.kind] ?? p.kind}</Label></SplitItem>
                <SplitItem isFilled>{p.subject}</SplitItem>
                <SplitItem><code>{short(p.orig_sha)}</code></SplitItem>
              </Split>
            </CardTitle>
            <CardBody>
              <Stack hasGutter>
                <StackItem>
                  <Content component="small">{p.author} - {p.note}</Content>
                </StackItem>
                {p.kind === "evil_merge" ? (
                  <StackItem>
                    <Alert variant="warning" isInline isPlain title="This merge commit contains changes that exist only in the merge itself.">
                      They cannot be cherry-picked. Acknowledge to continue without them - if they are still needed,
                      add them to this step's patch queue or as a new carry.
                    </Alert>
                    <ExpandableSection toggleText="Merge-only changes (combined diff)">
                      <LogTail log={p.detail} maxHeight={400} />
                    </ExpandableSection>
                  </StackItem>
                ) : (
                  <StackItem>
                    {(QUESTION_OPTIONS[p.question] ?? []).map((o) => (
                      <Radio
                        key={o.value}
                        id={`${p.orig_sha}-${o.value}`}
                        name={`decision-${p.orig_sha}`}
                        label={o.label}
                        isChecked={answers[p.orig_sha]?.decision === o.value}
                        onChange={() => set(p.orig_sha, "decision", o.value)}
                      />
                    ))}
                  </StackItem>
                )}
                {p.kind === "evil_merge" && (
                  <StackItem>
                    <Radio
                      id={`${p.orig_sha}-ack`}
                      name={`decision-${p.orig_sha}`}
                      label="Acknowledge - continue without the merge-only changes"
                      isChecked={answers[p.orig_sha]?.decision === "drop"}
                      onChange={() => set(p.orig_sha, "decision", "drop")}
                    />
                  </StackItem>
                )}
                <StackItem>
                  <TextInput
                    value={answers[p.orig_sha]?.reason ?? ""}
                    onChange={(_, v) => set(p.orig_sha, "reason", v)}
                    placeholder="Reason (required)"
                    aria-label="Reason"
                  />
                </StackItem>
              </Stack>
            </CardBody>
          </Card>
        </StackItem>
      ))}
      <StackItem>
        <Button variant="primary" onClick={() => onDone(save)} isDisabled={busy || !complete} isLoading={busy}>
          Save decisions and continue
        </Button>
      </StackItem>
    </Stack>
  );
}

function ConflictForm({ pluginApi, slotContext, conflictId, busy, who, onDone }) {
  const { workflowId, stepId, instanceId, runId } = slotContext ?? {};
  const [data, setData] = useState(null);
  const [error, setError] = useState(null);
  const [choices, setChoices] = useState({}); // path → { mode: edit|ours|theirs|delete, content }
  const [skip, setSkip] = useState(false);
  const [reason, setReason] = useState("");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await pluginApi.execute("conflict_get", { conflict_id: conflictId }, { instanceId, workflowId, stepId, runId });
        if (cancelled) return;
        setData(res);
        const init = {};
        (res.conflict?.files ?? []).forEach((f) => {
          init[f.path] = f.binary
            ? { mode: "ours", content: "" }
            : { mode: "edit", content: f.suggestion ?? f.merged ?? "" };
        });
        setChoices(init);
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [conflictId]); // eslint-disable-line react-hooks/exhaustive-deps

  if (error) return <Alert variant="danger" isInline title="Could not load the conflict">{error}</Alert>;
  if (!data) return <em>Loading conflict…</em>;

  const c = data.conflict;
  const files = c.files ?? [];
  const set = (path, patch) => setChoices((s) => ({ ...s, [path]: { ...s[path], ...patch } }));
  const hasMarkers = (s) => /^<{7}( |$)/m.test(s) && /^>{7}( |$)/m.test(s);
  const unresolved = files.filter((f) => choices[f.path]?.mode === "edit" && hasMarkers(choices[f.path]?.content ?? ""));
  const canSave = skip ? reason.trim().length > 0 : unresolved.length === 0;

  const save = () => {
    const body = { conflict_id: conflictId, resolved_by: who, reason };
    if (skip) {
      body.kind = "skip";
    } else {
      body.kind = "files";
      body.files = {};
      files.forEach((f) => {
        const ch = choices[f.path];
        if (ch.mode === "edit") body.files[f.path] = { content: ch.content };
        else if (ch.mode === "delete") body.files[f.path] = { deleted: true };
        else body.files[f.path] = { take: ch.mode };
      });
    }
    return pluginApi.execute("resolution_save", body, { instanceId, workflowId, stepId, runId });
  };

  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">
          Commit <strong>#{c.ordinal}</strong> <code>{short(c.orig_sha)}</code> - {c.subject} - conflicts with the new
          upstream base. <em>Ours</em> is the new upstream side (plus commits already re-applied); <em>theirs</em> is the
          fork commit being picked. The answer is saved and re-applied automatically whenever this exact conflict recurs.
        </Content>
      </StackItem>
      {data.hints?.length > 0 && (
        <StackItem>
          <Alert variant="info" isInline title={`This commit was resolved ${data.hints.length} time(s) before against a different base`}>
            Earlier resolutions are shown per file below as a starting point.
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <Radio id="mode-files" name="res-mode" label="Resolve the files" isChecked={!skip} onChange={() => setSkip(false)} />
        <Radio id="mode-skip" name="res-mode" label="Skip this commit entirely (it's no longer needed on this base)"
          isChecked={skip} onChange={() => setSkip(true)} />
      </StackItem>
      {!skip &&
        files.map((f) => (
          <StackItem key={f.path}>
            <FileResolver
              file={f}
              choice={choices[f.path] ?? {}}
              onChange={(patch) => set(f.path, patch)}
              hints={(data.hints ?? []).map((h) => ({ id: h.resolution_id, at: h.updated_at, file: h.files?.[f.path] })).filter((h) => h.file)}
              hasMarkers={hasMarkers}
            />
          </StackItem>
        ))}
      <StackItem>
        <FormGroup label={skip ? "Reason (required)" : "Note (optional)"} fieldId="res-reason">
          <TextInput id="res-reason" value={reason} onChange={(_, v) => setReason(v)} />
        </FormGroup>
      </StackItem>
      <StackItem>
        {unresolved.length > 0 && !skip && (
          <Alert variant="warning" isInline isPlain title={`Still contains conflict markers: ${unresolved.map((f) => f.path).join(", ")}`} />
        )}
        <Form onSubmit={(e) => e.preventDefault()}>
          <ActionGroup>
            <Button variant="primary" onClick={() => onDone(save)} isDisabled={busy || !canSave} isLoading={busy}>
              Save resolution and continue
            </Button>
          </ActionGroup>
        </Form>
      </StackItem>
    </Stack>
  );
}

function SidePane({ title, value, binary, path }) {
  return (
    <div style={{ flex: 1, minWidth: 0 }}>
      <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>{title}</div>
      {value == null ? (
        <em style={{ fontSize: 12, color: "#6a6e73" }}>(file absent on this side)</em>
      ) : binary ? (
        <em style={{ fontSize: 12, color: "#6a6e73" }}>(binary, {Math.round((value.length * 3) / 4)} bytes)</em>
      ) : (
        <CodeEditor value={value} path={path} readOnly height={320} />
      )}
    </div>
  );
}

function FileResolver({ file, choice, onChange, hints, hasMarkers }) {
  const modes = [
    ...(file.binary ? [] : [{ v: "edit", l: "Edit the merged result" }]),
    ...(file.ours != null ? [{ v: "ours", l: "Take upstream (ours)" }] : []),
    ...(file.theirs != null ? [{ v: "theirs", l: "Take the fork commit (theirs)" }] : []),
    { v: "delete", l: "Delete the file" },
  ];
  const markers = choice.mode === "edit" && hasMarkers(choice.content ?? "");
  return (
    <Card isCompact>
      <CardTitle>
        <Split hasGutter style={{ alignItems: "center" }}>
          <SplitItem isFilled><code>{file.path}</code></SplitItem>
          {file.suggestion != null && <SplitItem><Label isCompact color="teal">rerere suggestion pre-filled</Label></SplitItem>}
          {markers && <SplitItem><Label isCompact color="red">markers</Label></SplitItem>}
        </Split>
      </CardTitle>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <Split hasGutter style={{ flexWrap: "wrap" }}>
              {modes.map((m) => (
                <SplitItem key={m.v}>
                  <Radio
                    id={`${file.path}-${m.v}`}
                    name={`mode-${file.path}`}
                    label={m.l}
                    isChecked={choice.mode === m.v}
                    onChange={() => onChange({ mode: m.v })}
                  />
                </SplitItem>
              ))}
            </Split>
          </StackItem>
          {choice.mode === "edit" && (
            <StackItem>
              <CodeEditor
                value={choice.content ?? ""}
                path={file.path}
                onChange={(v) => onChange({ content: v })}
                height={460}
              />
              <Split hasGutter style={{ marginTop: 6, flexWrap: "wrap" }}>
                {file.merged != null && (
                  <SplitItem><Button variant="link" isInline onClick={() => onChange({ content: file.merged })}>Reset to conflict markers</Button></SplitItem>
                )}
                {file.suggestion != null && (
                  <SplitItem><Button variant="link" isInline onClick={() => onChange({ content: file.suggestion })}>Use rerere suggestion</Button></SplitItem>
                )}
                {file.ours != null && (
                  <SplitItem><Button variant="link" isInline onClick={() => onChange({ content: file.ours })}>Start from upstream</Button></SplitItem>
                )}
                {file.theirs != null && (
                  <SplitItem><Button variant="link" isInline onClick={() => onChange({ content: file.theirs })}>Start from fork commit</Button></SplitItem>
                )}
                {hints.filter((h) => h.file.content != null).map((h) => (
                  <SplitItem key={h.id}>
                    <Button variant="link" isInline onClick={() => onChange({ content: h.file.content })}>
                      Use earlier resolution ({new Date(h.at).toLocaleDateString()})
                    </Button>
                  </SplitItem>
                ))}
              </Split>
            </StackItem>
          )}
          <StackItem>
            <ExpandableSection toggleText="Base / upstream / fork versions">
              <div style={{ display: "flex", gap: 8 }}>
                <SidePane title="Base (common ancestor)" value={file.base} binary={file.binary} path={file.path} />
                <SidePane title="Upstream (ours)" value={file.ours} binary={file.binary} path={file.path} />
                <SidePane title="Fork commit (theirs)" value={file.theirs} binary={file.binary} path={file.path} />
              </div>
            </ExpandableSection>
          </StackItem>
        </Stack>
      </CardBody>
    </Card>
  );
}

// ── workflow-step-config ──────────────────────────────────────────────────────

function StepConfigPanel(props) {
  const [tab, setTab] = useState("patches");
  return (
    <Tabs activeKey={tab} onSelect={(_, k) => setTab(k)} aria-label="Step configuration">
      <Tab eventKey="patches" title={<TabTitleText>Patches</TabTitleText>}>
        <div style={{ paddingTop: 16 }}>
          <PatchQueueEditor
            {...props}
            intro={
              <>
                User-defined commits for step <code>{props.slotContext?.stepId}</code>, applied after the fork's
                commits are re-picked and <code>go mod vendor</code> has run, before each dir's <code>make</code>{" "}
                targets - on every run of workflow <code>{props.slotContext?.workflowId}</code>. Each resulting
                commit carries a <code>Rebase-Patch-Id</code> trailer, so the next rebase re-applies it from this
                queue instead of cherry-picking it twice.
              </>
            }
          />
        </div>
      </Tab>
      <Tab eventKey="memory" title={<TabTitleText>Memory</TabTitleText>}>
        <div style={{ paddingTop: 16 }}>
          <MemoryPanel {...props} />
        </div>
      </Tab>
    </Tabs>
  );
}

// MemoryPanel: everything this step remembers between runs - saved
// conflict resolutions, carry/drop decisions, and the git rerere cache -
// with selective deletion and a scoped wipe (which can also clear the
// patch queue and finished run history).
function MemoryPanel({ pluginApi, slotContext }) {
  const { workflowId, stepId, instanceId } = slotContext ?? {};
  const opts = { instanceId, workflowId, stepId };
  const [res, setRes] = useState(null);
  const [dec, setDec] = useState(null);
  const [rr, setRr] = useState(null);
  const [selRes, setSelRes] = useState(new Set());
  const [selDec, setSelDec] = useState(new Set());
  const [selRr, setSelRr] = useState(new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [done, setDone] = useState(null);

  const reload = async () => {
    try {
      const [r, d, e] = await Promise.all([
        pluginApi.execute("resolutions_list", {}, opts),
        pluginApi.execute("decisions_list", {}, opts),
        pluginApi.execute("rerere_list", {}, opts),
      ]);
      setRes(r.resolutions ?? []);
      setDec(d.decisions ?? []);
      setRr(e.entries ?? []);
      setSelRes(new Set());
      setSelDec(new Set());
      setSelRr(new Set());
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  };
  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workflowId, stepId, instanceId]);

  const decKey = (d) => `${d.orig_sha}/${d.question}`;
  // Rerere entries that go with the selected resolutions.
  const cascaded = new Set((rr ?? []).filter((e) => e.resolution_id && selRes.has(e.resolution_id)).map((e) => e.id));
  const total = selRes.size + selDec.size + selRr.size;

  const deleteSelected = async () => {
    const extra = cascaded.size > 0 ? ` (plus ${cascaded.size} linked rerere entr${cascaded.size === 1 ? "y" : "ies"})` : "";
    if (!window.confirm(`Delete ${total} selected item(s)${extra}? Future runs that hit them will ask again.`)) return;
    setBusy(true);
    setError(null);
    try {
      const out = await pluginApi.execute(
        "memory_delete",
        {
          resolution_ids: [...selRes],
          decisions: (dec ?? []).filter((d) => selDec.has(decKey(d))).map((d) => ({ orig_sha: d.orig_sha, question: d.question })),
          rerere_ids: [...selRr].filter((id) => !cascaded.has(id)),
        },
        opts,
      );
      setDone(out.deleted);
      await reload();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const toggle = (set, setter, key) => {
    const next = new Set(set);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    setter(next);
  };

  return (
    <Stack hasGutter>
      {error && <StackItem><Alert variant="danger" isInline title="Error">{error}</Alert></StackItem>}
      {done && (
        <StackItem>
          <Alert variant="success" isInline title="Deleted" actionClose={<Button variant="plain" onClick={() => setDone(null)}>×</Button>}>
            {Object.entries(done).filter(([, n]) => n > 0).map(([k, n]) => `${n} ${k.replaceAll("_", " ")}`).join(" · ") || "nothing matched"}
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <Content component="small">
          What this step remembers between runs. Saved resolutions are re-applied whenever the exact same conflict
          recurs; decisions answer carry/drop questions; the rerere cache feeds suggestions (or auto-resolution with{" "}
          <code>rerere: auto</code>). Deleting a resolution also deletes the rerere entries it produced.
        </Content>
      </StackItem>

      <StackItem>
        <MemorySection
          title="Conflict resolutions"
          items={res}
          keyOf={(r) => r.resolution_id}
          selected={selRes}
          onToggle={(k) => toggle(selRes, setSelRes, k)}
          onAll={(keys) => setSelRes(new Set(keys))}
          render={(r) => {
            const linked = (rr ?? []).filter((e) => e.resolution_id === r.resolution_id).length;
            return (
              <>
                <Label isCompact color={r.kind === "skip" ? "orange" : "teal"}>{r.kind}</Label>{" "}
                {r.subject}
                <div style={{ fontSize: 11, color: "#6a6e73" }}>
                  {Object.keys(r.files ?? {}).join(", ") || r.reason} · by {r.resolved_by} · used {r.use_count}× ·{" "}
                  {linked} rerere entr{linked === 1 ? "y" : "ies"} · {new Date(r.updated_at).toLocaleString()}
                </div>
              </>
            );
          }}
        />
      </StackItem>
      <StackItem>
        <MemorySection
          title="Decisions"
          items={dec}
          keyOf={decKey}
          selected={selDec}
          onToggle={(k) => toggle(selDec, setSelDec, k)}
          onAll={(keys) => setSelDec(new Set(keys))}
          render={(d) => (
            <>
              <Label isCompact>{d.question}: {d.decision}</Label>{" "}
              {d.subject || <code>{short(d.orig_sha)}</code>}
              <div style={{ fontSize: 11, color: "#6a6e73" }}>
                <code>{short(d.orig_sha)}</code> · {d.reason} · by {d.decided_by}
              </div>
            </>
          )}
        />
      </StackItem>
      <StackItem>
        <MemorySection
          title="git rerere cache"
          items={rr}
          keyOf={(e) => e.id}
          selected={new Set([...selRr, ...cascaded])}
          locked={cascaded}
          onToggle={(k) => !cascaded.has(k) && toggle(selRr, setSelRr, k)}
          onAll={(keys) => setSelRr(new Set(keys))}
          render={(e) => (
            <>
              <code>{e.path || "(path unknown)"}</code>{" "}
              {e.resolution_id ? (
                <Label isCompact color="teal">from resolution</Label>
              ) : e.subject ? (
                <Label isCompact color="purple">rerere auto</Label>
              ) : (
                <Label isCompact color="grey">unlinked</Label>
              )}
              {cascaded.has(e.id) && <> <Label isCompact color="red">deleted with its resolution</Label></>}
              <div style={{ fontSize: 11, color: "#6a6e73" }}>
                {e.subject || "origin not recorded"} · <code>{e.id.slice(0, 12)}</code> ·{" "}
                {new Date(e.updated_at).toLocaleString()}
              </div>
            </>
          )}
        />
      </StackItem>
      <StackItem>
        <Button variant="danger" isDisabled={busy || total === 0} isLoading={busy} onClick={deleteSelected}>
          Delete selected ({total + [...cascaded].filter((id) => !selRr.has(id)).length})
        </Button>
      </StackItem>

      <StackItem>
        <WipeCard pluginApi={pluginApi} opts={opts} workflowId={workflowId} stepId={stepId}
          onDone={(counts) => { setDone(counts); reload(); }} onError={setError} />
      </StackItem>
    </Stack>
  );
}

function MemorySection({ title, items, keyOf, selected, locked, onToggle, onAll, render }) {
  const list = items ?? [];
  const allSelected = list.length > 0 && list.every((it) => selected.has(keyOf(it)));
  return (
    <Card isCompact>
      <CardTitle>
        <Split hasGutter style={{ alignItems: "center" }}>
          <SplitItem isFilled>{title} ({items == null ? "…" : list.length})</SplitItem>
          {list.length > 0 && (
            <SplitItem>
              <Button variant="link" isInline onClick={() => onAll(allSelected ? [] : list.map(keyOf))}>
                {allSelected ? "Select none" : "Select all"}
              </Button>
            </SplitItem>
          )}
        </Split>
      </CardTitle>
      <CardBody>
        {list.length === 0 ? (
          <em style={{ color: "#6a6e73" }}>None saved.</em>
        ) : (
          <Stack hasGutter>
            {list.map((it) => {
              const k = keyOf(it);
              return (
                <StackItem key={k}>
                  <Checkbox
                    id={`mem-${title}-${k}`}
                    isChecked={selected.has(k)}
                    isDisabled={locked?.has(k)}
                    onChange={() => onToggle(k)}
                    label={<div>{render(it)}</div>}
                  />
                </StackItem>
              );
            })}
          </Stack>
        )}
      </CardBody>
    </Card>
  );
}

const WIPE_CATEGORIES = [
  { key: "resolutions", label: "Saved conflict resolutions (and the rerere entries they produced)" },
  { key: "decisions", label: "Carry / drop decisions" },
  { key: "rerere", label: "Entire git rerere cache" },
  { key: "patches", label: "Patch queue (user-defined commits)" },
  { key: "run_history", label: "Finished run history (commit tables, conflicts) - running or suspended runs are kept" },
];

function WipeCard({ pluginApi, opts, workflowId, stepId, onDone, onError }) {
  const [scope, setScope] = useState("step");
  const [cats, setCats] = useState({ resolutions: true, decisions: true, rerere: true, patches: false, run_history: false });
  const [busy, setBusy] = useState(false);
  const chosen = WIPE_CATEGORIES.filter((c) => cats[c.key]);

  const wipe = async () => {
    const where = scope === "workflow" ? `every step of workflow "${workflowId}"` : `step "${stepId}"`;
    if (!window.confirm(`Wipe ${chosen.map((c) => c.key.replaceAll("_", " ")).join(", ")} for ${where}? This cannot be undone.`)) return;
    setBusy(true);
    try {
      const out = await pluginApi.execute("memory_wipe", { scope, categories: cats }, opts);
      onDone(out.deleted);
    } catch (e) {
      onError(String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card isCompact style={{ borderTop: "3px solid #c9190b" }}>
      <CardTitle>Wipe memory</CardTitle>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <Radio id="wipe-step" name="wipe-scope" label={<>This step only (<code>{stepId}</code>)</>}
              isChecked={scope === "step"} onChange={() => setScope("step")} />
            <Radio id="wipe-workflow" name="wipe-scope" label={<>Every rebase step in workflow <code>{workflowId}</code></>}
              isChecked={scope === "workflow"} onChange={() => setScope("workflow")} />
          </StackItem>
          <StackItem>
            {WIPE_CATEGORIES.map((c) => (
              <Checkbox key={c.key} id={`wipe-${c.key}`} label={c.label} isChecked={!!cats[c.key]}
                onChange={(_, v) => setCats((x) => ({ ...x, [c.key]: v }))} />
            ))}
          </StackItem>
          <StackItem>
            <Button variant="danger" onClick={wipe} isDisabled={busy || chosen.length === 0} isLoading={busy}>
              Wipe…
            </Button>
          </StackItem>
        </Stack>
      </CardBody>
    </Card>
  );
}
