// CodeEditor: a CodeMirror 6 editor (One Dark theme) for the conflict
// resolver. Bundled into this plugin's MFE - not shared with the shell.
//
//   <CodeEditor value path onChange readOnly height />
//
// `path` picks syntax highlighting by file extension. Conflict regions
// are tinted: the upstream ("ours") side green, the fork ("theirs")
// side blue, a diff3 base section grey, and the marker lines red, so an
// unresolved hunk is obvious at a glance.

import { useEffect, useRef } from "react";
import { basicSetup } from "codemirror";
import { EditorState, Compartment, RangeSetBuilder } from "@codemirror/state";
import { Decoration, EditorView, ViewPlugin } from "@codemirror/view";
import { oneDark } from "@codemirror/theme-one-dark";
import { go } from "@codemirror/lang-go";
import { json } from "@codemirror/lang-json";
import { yaml } from "@codemirror/lang-yaml";
import { markdown } from "@codemirror/lang-markdown";

function languageFor(path) {
  const p = (path ?? "").toLowerCase();
  if (p.endsWith(".go")) return go();
  if (p.endsWith(".json")) return json();
  if (p.endsWith(".yaml") || p.endsWith(".yml")) return yaml();
  if (p.endsWith(".md") || p.endsWith(".markdown")) return markdown();
  return [];
}

// ── conflict-region highlighting ──────────────────────────────────────────────

const markerLine = Decoration.line({ class: "cm-conflict-marker" });
const oursLine = Decoration.line({ class: "cm-conflict-ours" });
const baseLine = Decoration.line({ class: "cm-conflict-base" });
const theirsLine = Decoration.line({ class: "cm-conflict-theirs" });

// Walks the whole document (conflicted files are small enough) and
// classifies each line by where it sits relative to git's markers:
//   <<<<<<< ours … ||||||| base … ======= … >>>>>>> theirs
function conflictDecorations(doc) {
  const b = new RangeSetBuilder();
  let region = null; // null | "ours" | "base" | "theirs"
  for (let i = 1; i <= doc.lines; i++) {
    const line = doc.line(i);
    const t = line.text;
    if (/^<{7}( |$)/.test(t)) {
      region = "ours";
      b.add(line.from, line.from, markerLine);
    } else if (region && /^\|{7}( |$)/.test(t)) {
      region = "base";
      b.add(line.from, line.from, markerLine);
    } else if (region && /^={7}$/.test(t)) {
      region = "theirs";
      b.add(line.from, line.from, markerLine);
    } else if (region && /^>{7}( |$)/.test(t)) {
      region = null;
      b.add(line.from, line.from, markerLine);
    } else if (region === "ours") {
      b.add(line.from, line.from, oursLine);
    } else if (region === "base") {
      b.add(line.from, line.from, baseLine);
    } else if (region === "theirs") {
      b.add(line.from, line.from, theirsLine);
    }
  }
  return b.finish();
}

const conflictHighlighter = ViewPlugin.fromClass(
  class {
    constructor(view) {
      this.decorations = conflictDecorations(view.state.doc);
    }
    update(u) {
      if (u.docChanged) this.decorations = conflictDecorations(u.state.doc);
    }
  },
  { decorations: (v) => v.decorations },
);

const conflictTheme = EditorView.theme({
  ".cm-conflict-marker": { backgroundColor: "rgba(224, 108, 117, 0.28)", fontWeight: "bold" },
  ".cm-conflict-ours": { backgroundColor: "rgba(152, 195, 121, 0.14)" },
  ".cm-conflict-base": { backgroundColor: "rgba(171, 178, 191, 0.10)" },
  ".cm-conflict-theirs": { backgroundColor: "rgba(97, 175, 239, 0.14)" },
});

// ── component ─────────────────────────────────────────────────────────────────

export function CodeEditor({ value, path, onChange, readOnly = false, height = 420 }) {
  const host = useRef(null);
  const view = useRef(null);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const lang = useRef(new Compartment());

  // Create the editor once per mount.
  useEffect(() => {
    const v = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: value ?? "",
        extensions: [
          basicSetup,
          oneDark,
          conflictTheme,
          conflictHighlighter,
          lang.current.of(languageFor(path)),
          EditorState.readOnly.of(readOnly),
          EditorView.editable.of(!readOnly),
          EditorView.theme({
            "&": { height: `${height}px`, fontSize: "12px", borderRadius: "4px" },
            ".cm-scroller": { fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace" },
          }),
          EditorView.updateListener.of((u) => {
            if (u.docChanged && onChangeRef.current) onChangeRef.current(u.state.doc.toString());
          }),
        ],
      }),
    });
    view.current = v;
    return () => {
      v.destroy();
      view.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [readOnly, height]);

  // Accept content replaced from outside ("Use rerere suggestion", …)
  // without fighting the user's own typing.
  useEffect(() => {
    const v = view.current;
    if (!v) return;
    const cur = v.state.doc.toString();
    const next = value ?? "";
    if (cur !== next) v.dispatch({ changes: { from: 0, to: cur.length, insert: next } });
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({ effects: lang.current.reconfigure(languageFor(path)) });
  }, [path]);

  return <div ref={host} style={{ border: "1px solid #3b4048", borderRadius: 4, overflow: "hidden" }} />;
}
