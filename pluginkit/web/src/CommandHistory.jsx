// Complete command history of one (workflow_run_id, step_id) - the UI
// half of pluginkit/cmdlog. Every command, read-only query, and note the
// step's runner produced, across every execution (first run, each
// resume after a suspend, each retry), with exit code, timing, and the
// full output of any entry on demand. Round-trips exclusively through
// pluginApi.execute's cmdlog_list / cmdlog_get with run + step scoping.
//
// Props:
//   pluginApi, scope = { instanceId, workflowId, stepId, runId }
//   liveKey   any value that changes while the step is producing output
//             (e.g. the latest progress payload); each change pulls
//             new / still-running entries, throttled. Omit for a
//             finished step.
//   maxHeight scroll height of the list.
import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, Button, Checkbox, Label, Spinner, Split, SplitItem, Stack, StackItem, TextInput } from "@patternfly/react-core";
import { LogTail } from "./fields.jsx";

const PAGE = 500;
const LIVE_THROTTLE_MS = 1500;
const KIND_TEXT = { command: "Commands", query: "Queries", note: "Notes" };

function fmtBytes(n) {
  if (!n) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function fmtDuration(a, b) {
  if (!a || !b) return "";
  const ms = new Date(b) - new Date(a);
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  return `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`;
}

function Status({ e }) {
  if (e.kind === "note") return <span style={{ color: "#6a6e73" }}>#</span>;
  if (!e.finished_at) return <Spinner size="sm" aria-label="running" />;
  if (e.exit_code === 0) return <span style={{ color: "#3e8635" }} title="exit 0">✓</span>;
  return (
    <span style={{ color: "#c9190b", fontWeight: 600 }} title={e.error || `exit ${e.exit_code}`}>
      ✗ {e.exit_code}
    </span>
  );
}

export function CommandHistory({ pluginApi, scope, liveKey, maxHeight = 520 }) {
  const { instanceId, workflowId, stepId, runId } = scope ?? {};
  const [kinds, setKinds] = useState({ command: true, query: false, note: true });
  const [failedOnly, setFailedOnly] = useState(false);
  const [qInput, setQInput] = useState("");
  const [q, setQ] = useState("");
  const [entries, setEntries] = useState([]); // sorted by id
  const [counts, setCounts] = useState({});
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);
  const [open, setOpen] = useState({}); // id → { loading, entry, error }
  const lastLive = useRef(0);
  const entriesRef = useRef(entries);
  entriesRef.current = entries;

  const opts = { instanceId, workflowId, stepId, runId };
  const selectedKinds = Object.keys(kinds).filter((k) => kinds[k]);
  const filterKey = `${runId}|${stepId}|${selectedKinds.join(",")}|${q}`;

  const fetchPage = useCallback(
    (afterID) =>
      pluginApi.execute("cmdlog_list", { after_id: afterID, limit: PAGE, kinds: selectedKinds, q }, opts),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filterKey, instanceId],
  );

  const merge = (rows) =>
    setEntries((prev) => {
      const m = new Map(prev.map((e) => [e.id, e]));
      rows.forEach((e) => m.set(e.id, e));
      return [...m.values()].sort((a, b) => a.id - b.id);
    });

  // Filters changed: start over.
  useEffect(() => {
    if (!runId || !stepId) return;
    let cancelled = false;
    setLoading(true);
    setEntries([]);
    setOpen({});
    fetchPage(0)
      .then((res) => {
        if (cancelled) return;
        setEntries(res.entries ?? []);
        setCounts(res.counts ?? {});
        setMore(Boolean(res.more));
        setError(null);
      })
      .catch((e) => !cancelled && setError(String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [fetchPage, runId, stepId]);

  // Live: pull everything from the oldest still-running entry onward.
  useEffect(() => {
    if (liveKey === undefined || !runId || loading || more) return;
    const now = Date.now();
    if (now - lastLive.current < LIVE_THROTTLE_MS) return;
    lastLive.current = now;
    const cur = entriesRef.current;
    const unfinished = cur.find((e) => !e.finished_at && e.kind !== "note");
    const after = unfinished ? unfinished.id - 1 : cur.length ? cur[cur.length - 1].id : 0;
    fetchPage(after)
      .then((res) => {
        merge(res.entries ?? []);
        setCounts(res.counts ?? {});
        setMore(Boolean(res.more));
      })
      .catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [liveKey]);

  // Debounced server-side search.
  useEffect(() => {
    const t = setTimeout(() => setQ(qInput.trim()), 400);
    return () => clearTimeout(t);
  }, [qInput]);

  const loadMore = async () => {
    setLoading(true);
    try {
      const res = await fetchPage(entries.length ? entries[entries.length - 1].id : 0);
      merge(res.entries ?? []);
      setMore(Boolean(res.more));
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  };

  const toggle = async (e) => {
    if (open[e.id]) {
      setOpen(({ [e.id]: _, ...rest }) => rest);
      return;
    }
    if (e.kind === "note") {
      setOpen((o) => ({ ...o, [e.id]: { entry: { ...e, output: e.command } } }));
      return;
    }
    setOpen((o) => ({ ...o, [e.id]: { loading: true } }));
    try {
      const res = await pluginApi.execute("cmdlog_get", { id: e.id }, opts);
      setOpen((o) => ({ ...o, [e.id]: { entry: res.entry } }));
    } catch (err) {
      setOpen((o) => ({ ...o, [e.id]: { error: String(err) } }));
    }
  };

  if (!runId || !stepId) return null;

  const shown = failedOnly
    ? entries.filter((e) => e.finished_at && e.exit_code !== 0 && e.kind !== "note")
    : entries;
  const execOrder = [...new Set(entries.map((e) => e.execution_id))];
  let lastExec = null;

  return (
    <Stack hasGutter>
      <StackItem>
        <Split hasGutter style={{ flexWrap: "wrap", alignItems: "center" }}>
          {Object.keys(KIND_TEXT).map((k) => (
            <SplitItem key={k}>
              <Checkbox
                id={`cmdlog-kind-${stepId}-${k}`}
                label={`${KIND_TEXT[k]} (${counts[k] ?? 0})`}
                isChecked={kinds[k]}
                onChange={(_, v) => setKinds((s) => ({ ...s, [k]: v }))}
              />
            </SplitItem>
          ))}
          <SplitItem>
            <Checkbox
              id={`cmdlog-failed-${stepId}`}
              label="Failed only"
              isChecked={failedOnly}
              onChange={(_, v) => setFailedOnly(v)}
            />
          </SplitItem>
          <SplitItem isFilled>
            <TextInput
              value={qInput}
              onChange={(_, v) => setQInput(v)}
              placeholder="Search commands and output"
              aria-label="Search command history"
            />
          </SplitItem>
          <SplitItem style={{ color: "#6a6e73", fontSize: 12 }}>
            {shown.length}
            {more ? "+" : ""} shown
          </SplitItem>
        </Split>
      </StackItem>
      {error && (
        <StackItem>
          <Alert variant="warning" isInline title="Could not load the command history">{error}</Alert>
        </StackItem>
      )}
      <StackItem>
        <div style={{ maxHeight, overflowY: "auto", fontSize: 12 }}>
          {loading && entries.length === 0 && <Spinner size="md" aria-label="Loading command history" />}
          {!loading && shown.length === 0 && !error && (
            <em style={{ color: "#6a6e73" }}>No matching entries.</em>
          )}
          {shown.map((e) => {
            const header =
              e.execution_id !== lastExec ? (
                <div
                  key={`x-${e.execution_id}`}
                  style={{ margin: "8px 0 4px", color: "#6a6e73", borderBottom: "1px solid #d2d2d2" }}
                >
                  Execution {execOrder.indexOf(e.execution_id) + 1}
                  {e.attempt ? ` · attempt ${e.attempt}` : ""} · started {new Date(e.started_at).toLocaleString()}
                </div>
              ) : null;
            lastExec = e.execution_id;
            const o = open[e.id];
            return [
              header,
              <div key={e.id} style={{ borderBottom: "1px solid #f0f0f0" }}>
                <div
                  role="button"
                  tabIndex={0}
                  onClick={() => toggle(e)}
                  onKeyDown={(ev) => (ev.key === "Enter" || ev.key === " ") && toggle(e)}
                  style={{ display: "flex", gap: 8, padding: "3px 0", cursor: "pointer", alignItems: "baseline" }}
                  title={e.cwd ? `cwd: ${e.cwd}` : undefined}
                >
                  <span style={{ width: 16, color: "#6a6e73" }}>{o ? "▾" : "▸"}</span>
                  <span style={{ width: 44, textAlign: "right" }}><Status e={e} /></span>
                  {e.kind === "query" && <Label isCompact color="grey">query</Label>}
                  <code style={{ flex: 1, whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                    {e.kind === "note" ? e.command : `$ ${e.command}`}
                  </code>
                  <span style={{ color: "#6a6e73", whiteSpace: "nowrap" }}>
                    {fmtDuration(e.started_at, e.finished_at)} {fmtBytes(e.output_bytes)}
                  </span>
                </div>
                {o && (
                  <div style={{ padding: "4px 0 8px 24px" }}>
                    {o.loading && <Spinner size="sm" aria-label="Loading output" />}
                    {o.error && <Alert variant="warning" isInline isPlain title={o.error} />}
                    {o.entry && (
                      <>
                        {o.entry.cwd && (
                          <div style={{ color: "#6a6e73", marginBottom: 4 }}>
                            cwd <code>{o.entry.cwd}</code>
                            {o.entry.error ? <> · {o.entry.error.split("\n")[0]}</> : null}
                          </div>
                        )}
                        {o.entry.output ? (
                          <LogTail log={o.entry.output} maxHeight={500} />
                        ) : (
                          <em style={{ color: "#6a6e73" }}>
                            {o.entry.finished_at ? "No output." : "Still running - output is recorded when it finishes."}
                          </em>
                        )}
                      </>
                    )}
                  </div>
                )}
              </div>,
            ];
          })}
          {more && (
            <div style={{ padding: 8 }}>
              <Button variant="secondary" size="sm" onClick={loadMore} isDisabled={loading}>
                {loading ? "Loading…" : `Load ${PAGE} more`}
              </Button>
            </div>
          )}
        </div>
      </StackItem>
    </Stack>
  );
}
