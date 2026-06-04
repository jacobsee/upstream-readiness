// Thin wrapper the shell hands to each plugin panel. The plugin uses
// this object to round-trip through the orchestrator's HTTP surface.
//
//   execute(action, payload, { instanceId, workflowId, stepId, runId })
//     → invoke an action ad-hoc against a chosen instance. workflowId,
//       stepId, and runId are optional scoping the orchestrator forwards
//       to the plugin so it can key its own per-workflow / per-step /
//       per-run state (e.g. from the workflow-step-config, step-runtime,
//       and resolution slots).
//
//   listInstances()
//     → list this plugin's registered instances (the plugin's instance
//       picker uses this when running ad-hoc)
//
//   saveInstance({ id, display_name, config_blob })
//     → upsert an instance (POST when id is unset, PUT otherwise).
//       The orchestrator stores config_blob opaquely.
//
// Anything else the plugin's UI needs (validation, draft persistence,
// etc.) should be expressed as additional methods here, never by the
// plugin reaching outside this surface.
export function makePluginApi(pluginId) {
  return {
    pluginId,

    async execute(action, payload, opts = {}) {
      const body = { action, payload };
      if (opts.instanceId) body.instance_id = opts.instanceId;
      if (opts.workflowId) body.workflow_id = opts.workflowId;
      if (opts.stepId) body.step_id = opts.stepId;
      if (opts.runId) body.workflow_run_id = opts.runId;
      const r = await fetch(`/api/plugins/${pluginId}/execute`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`execute ${action} → ${r.status}: ${text}`);
      }
      return r.json();
    },

    async listInstances() {
      const r = await fetch(`/api/instances?plugin=${encodeURIComponent(pluginId)}`);
      if (!r.ok) throw new Error(`/api/instances → ${r.status}`);
      return (await r.json()) ?? [];
    },

    async saveInstance({ id, display_name, config_blob }) {
      const body = { plugin_id: pluginId, display_name, config_blob };
      const url = id ? `/api/instances/${encodeURIComponent(id)}` : "/api/instances";
      const method = id ? "PUT" : "POST";
      const r = await fetch(url, {
        method,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`save instance → ${r.status}: ${text}`);
      }
      return r.json();
    },
  };
}
