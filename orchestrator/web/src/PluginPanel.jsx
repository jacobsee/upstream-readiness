import { useEffect, useState } from "react";
import {
  Bullseye,
  EmptyState,
  EmptyStateBody,
  Spinner,
  Alert,
} from "@patternfly/react-core";
import { makePluginApi } from "./pluginApi.js";

// Dynamic-import + render of a plugin's React component. The plugin's
// bundle URL and the export name to mount both come from the manifest;
// the shell knows nothing else about the plugin's UI.
//
// `mode` selects which MFE slot the component is being mounted in:
//   "action"           - per-action runtime panel (default)
//   "instance-config"  - instance configuration form
//   "step-editor"      - workflow step params editor
//   "resolution"       - human-in-the-loop resolution prompt
//
// If the manifest declares a different mfe_element for this slot, that
// named export is mounted instead of the default. `slotContext` is
// forwarded to the component verbatim so each slot can carry its own
// shape (existing instance to edit, upstream outputs available, etc.).
export default function PluginPanel({ manifest, mode = "action", slotContext }) {
  const [Component, setComponent] = useState(null);
  const [error, setError] = useState(null);

  const slot = (manifest.mfeSlots ?? []).find((s) => s.slot === mode);
  const exportName = slot?.mfeElement || manifest.mfeElement || "default";

  useEffect(() => {
    let cancelled = false;
    setComponent(null);
    setError(null);
    (async () => {
      try {
        const mod = await import(/* @vite-ignore */ manifest.mfeUrl);
        const Exported = mod[exportName] ?? mod.default;
        if (typeof Exported !== "function") {
          throw new Error(
            `module did not export "${exportName}" as a React component`,
          );
        }
        if (!cancelled) setComponent(() => Exported);
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [manifest.id, manifest.mfeUrl, exportName]);

  if (error) {
    return (
      <Alert variant="danger" title={`Failed to load ${manifest.id}`}>
        {error}
      </Alert>
    );
  }
  if (!Component) {
    return (
      <Bullseye>
        <Spinner aria-label={`Loading ${manifest.displayName}`} />
      </Bullseye>
    );
  }

  return (
    <Component
      manifest={manifest}
      pluginApi={makePluginApi(manifest.id)}
      mode={mode}
      slotContext={slotContext}
    />
  );
}

export function NoPluginSelected() {
  return (
    <Bullseye>
      <EmptyState titleText="Select something" headingLevel="h4">
        <EmptyStateBody>
          Pick a plugin, workflow, or instance from the sidebar.
        </EmptyStateBody>
      </EmptyState>
    </Bullseye>
  );
}
