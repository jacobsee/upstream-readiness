import { useState } from "react";

// WorkflowGraph renders the step DAG as an SVG.
// Columns = dependency-depth layers. Independent steps occupy the same
// column and run in parallel. Edges show depends_on relationships.
// If a RunResult is passed, nodes are colour-coded by step outcome.

const NODE_W = 200;
const NODE_H = 72;
const COL_GAP = 100;
const ROW_GAP = 24;
const PAD = 44;
const COL_STRIDE = NODE_W + COL_GAP;
const ROW_STRIDE = NODE_H + ROW_GAP;

// Assign each step a column index equal to the longest dependency path
// from any root to that step. Steps with no deps are column 0.
function computeDepth(steps) {
  const memo = {};
  const stepMap = Object.fromEntries(steps.map(s => [s.id, s]));

  function depth(id) {
    if (id in memo) return memo[id];
    const step = stepMap[id];
    const deps = step?.depends_on ?? [];
    memo[id] = deps.length === 0 ? 0 : Math.max(...deps.map(depth)) + 1;
    return memo[id];
  }

  steps.forEach(s => depth(s.id));
  return memo;
}

function layout(steps) {
  const depthOf = computeDepth(steps);

  // Group step IDs by column, preserving declaration order within each column.
  const cols = {};
  steps.forEach(s => {
    const c = depthOf[s.id];
    (cols[c] ??= []).push(s.id);
  });

  const numCols = Math.max(...Object.values(depthOf)) + 1;
  const maxRows = Math.max(...Object.values(cols).map(c => c.length));

  const svgW = PAD * 2 + Math.max(numCols - 1, 0) * COL_STRIDE + NODE_W;
  const svgH = PAD * 2 + Math.max(maxRows - 1, 0) * ROW_STRIDE + NODE_H;

  const pos = {};
  Object.entries(cols).forEach(([col, ids]) => {
    const c = parseInt(col);
    // Centre each column vertically within the full height.
    const startY = PAD + ((maxRows - ids.length) / 2) * ROW_STRIDE;
    ids.forEach((id, row) => {
      pos[id] = {
        x: PAD + c * COL_STRIDE,
        y: startY + row * ROW_STRIDE,
      };
    });
  });

  // Bezier edges: right-centre of source → left-centre of target.
  const edges = [];
  steps.forEach(step => {
    (step.depends_on ?? []).forEach(depId => {
      const from = pos[depId];
      const to = pos[step.id];
      if (!from || !to) return;
      const x1 = from.x + NODE_W;
      const y1 = from.y + NODE_H / 2;
      const x2 = to.x;
      const y2 = to.y + NODE_H / 2;
      const mx = (x1 + x2) / 2;
      edges.push({ key: `${depId}→${step.id}`, x1, y1, x2, y2, mx });
    });
  });

  return { pos, edges, svgW, svgH };
}

const STATUS = {
  pending:   { fill: "#f5f5f5", stroke: "#8a8d90", label: "#6a6e73" },
  running:   { fill: "#e7f1fa", stroke: "#0066cc", label: "#1a1a1a" },
  // Suspended, waiting for the plugin's "resolution" slot to answer.
  awaiting_resolution: { fill: "#fdf7e7", stroke: "#b98412", label: "#1a1a1a" },
  succeeded: { fill: "#f0f9f0", stroke: "#1e8517", label: "#1a1a1a" },
  failed:    { fill: "#faeae8", stroke: "#c9190b", label: "#1a1a1a" },
  skipped:   { fill: "#fff8e7", stroke: "#f0ab00", label: "#1a1a1a" },
  cancelled: { fill: "#f0f0f0", stroke: "#6a6e73", label: "#6a6e73" },
};

function stepStatus(id, run) {
  if (!run) return "pending";
  const sr = run.steps?.find(s => s.step_id === id);
  const st = sr?.status;
  return STATUS[st] ? st : "pending";
}

// Material "edit" glyph (24×24), drawn inside the editable-step badge.
const PENCIL = "M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25zM20.71 7.04a1 1 0 0 0 0-1.41l-2.34-2.34a1 1 0 0 0-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83z";
const BADGE_R = 11;

// EditBadge sits on a node's top-right corner: the step's plugin has a
// workflow-step-config panel, so clicking the node opens it.
function EditBadge({ hovered }) {
  const icon = 14;
  return (
    <g transform={`translate(${NODE_W - 4},4)`} pointerEvents="none">
      <circle r={BADGE_R} fill={hovered ? "#0066cc" : "#fff"} stroke="#0066cc" strokeWidth={1.5} />
      <path
        d={PENCIL}
        fill={hovered ? "#fff" : "#0066cc"}
        transform={`translate(${-icon / 2},${-icon / 2}) scale(${icon / 24})`}
      />
    </g>
  );
}

// isStepEditable(step) says whether clicking the node opens the plugin's
// own per-step panel (workflow-step-config); those nodes get the badge.
export default function WorkflowGraph({ workflow, run, onStepClick, isStepEditable }) {
  const [hoveredID, setHoveredID] = useState(null);
  const steps = workflow?.steps ?? [];
  if (steps.length === 0) {
    return <p style={{ color: "#6a6e73", margin: 0 }}>No steps defined.</p>;
  }

  const { pos, edges, svgW, svgH } = layout(steps);

  return (
    <svg
      viewBox={`0 0 ${svgW} ${svgH}`}
      width={svgW}
      height={svgH}
      style={{ display: "block", maxWidth: "100%", fontFamily: "inherit" }}
    >
      <defs>
        <marker id="wf-arrow" markerWidth="7" markerHeight="5"
          refX="6" refY="2.5" orient="auto">
          <polygon points="0 0,7 2.5,0 5" fill="#8a8d90" />
        </marker>
      </defs>

      {/* Edges drawn before nodes so nodes sit on top */}
      {edges.map(e => (
        <path
          key={e.key}
          d={`M${e.x1},${e.y1} C${e.mx},${e.y1} ${e.mx},${e.y2} ${e.x2},${e.y2}`}
          fill="none"
          stroke="#8a8d90"
          strokeWidth={1.5}
          markerEnd="url(#wf-arrow)"
        />
      ))}

      {/* Nodes */}
      {steps.map(step => {
        const p = pos[step.id];
        if (!p) return null;
        const s = STATUS[stepStatus(step.id, run)];
        const clickable = typeof onStepClick === "function";
        const editable = clickable && Boolean(isStepEditable?.(step));
        const hovered = clickable && hoveredID === step.id;
        const open = () => onStepClick(step.id);
        return (
          <g
            key={step.id}
            transform={`translate(${p.x},${p.y})`}
            style={clickable ? { cursor: "pointer", outline: "none" } : undefined}
            onClick={clickable ? open : undefined}
            onMouseEnter={clickable ? () => setHoveredID(step.id) : undefined}
            onMouseLeave={clickable ? () => setHoveredID(null) : undefined}
            onFocus={clickable ? () => setHoveredID(step.id) : undefined}
            onBlur={clickable ? () => setHoveredID(null) : undefined}
            onKeyDown={clickable ? (e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                open();
              }
            } : undefined}
            tabIndex={clickable ? 0 : undefined}
            role={clickable ? "button" : undefined}
            aria-label={clickable ? `${editable ? "Configure" : "Open"} step ${step.id}` : undefined}
          >
            {clickable && <title>{editable ? `Configure ${step.id}` : step.id}</title>}
            <rect
              width={NODE_W} height={NODE_H} rx={6}
              fill={s.fill} stroke={s.stroke}
              strokeWidth={hovered ? 2.5 : 1.5}
              style={hovered ? { filter: "drop-shadow(0 2px 4px rgba(0,0,0,0.18))" } : undefined}
            />
            {/* Step ID */}
            <text x={NODE_W / 2} y={22}
              textAnchor="middle" fontSize={13} fontWeight={600}
              fill="#151515">
              {step.id}
            </text>
            {/* plugin */}
            <text x={NODE_W / 2} y={41}
              textAnchor="middle" fontSize={11} fill="#6a6e73">
              {step.plugin}
            </text>
            {/* action */}
            <text x={NODE_W / 2} y={57}
              textAnchor="middle" fontSize={11} fill={s.label}>
              /{step.action}
            </text>
            {editable && <EditBadge hovered={hovered} />}
          </g>
        );
      })}
    </svg>
  );
}
