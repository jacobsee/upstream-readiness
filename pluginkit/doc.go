// Package pluginkit is shared Go code for the repository's git/Go
// plugins (generic-go-dep, upstream-rebase). It holds the mechanics the
// plugins must behave identically on - SSH/git isolation, __ref
// resolution, the dependency bump → tidy → vendor → patches → make
// pipeline, the per-(workflow_id, step_id) patch queue, and the step's
// shell commands - so they
// can't drift apart. It also holds mfeassets, the Plugin.GetAsset
// implementation every plugin (greeter included) uses to serve its
// embedded MFE bundle through the orchestrator.
//
// Nothing here is orchestrator code: every package is consumed by plugin
// binaries only. Progress payload shapes stay plugin-owned (runner takes
// an emit callback), and storage stays per-plugin (patchqueue runs
// against whatever pool the plugin opened on its own database).
package pluginkit
