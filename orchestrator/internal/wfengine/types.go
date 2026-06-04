// Package wfengine wires the orchestrator's workflow execution layer
// to Temporal. The Temporal Workflow function (Execute) is a thin DAG
// driver that calls a single Activity (RunStep) for each plugin step;
// the Activity is the boundary across which all non-deterministic work
// (gRPC, DB lookups, retries) happens.
package wfengine

import (
	"encoding/json"
	"time"

	"github.com/jacobsee/upstream-readiness/orchestrator/internal/workflow"
)

// TaskQueue is the Temporal task queue the orchestrator's worker listens
// on and the HTTP layer schedules onto. A single queue is sufficient
// because all workflows are owned by the orchestrator itself; plugins
// don't host workers.
const TaskQueue = "upstream-readiness-workflows"

// TriggerInput is the payload passed to the Temporal workflow when an
// orchestrator HTTP request (or scheduled fire) starts a run.
// Everything the workflow function needs to drive the DAG must be in
// here: workflows must be deterministic, so they cannot read from
// Postgres or call gRPC outside Activities.
type TriggerInput struct {
	// RunID is the orchestrator's identifier for this run. It is also
	// used as the Temporal workflow ID so the two systems agree.
	RunID string `json:"run_id"`
	// The resolved workflow definition to execute.
	Definition workflow.Definition `json:"definition"`
	// Instance config blobs for every instance referenced by the
	// workflow's steps, keyed by instance ID. The HTTP handler that
	// starts the workflow loads these from Postgres and bundles them in.
	InstanceConfigs map[string]json.RawMessage `json:"instance_configs"`
}

// StepActivityInput is the payload passed to RunStep for a single step.
// It is fully self-contained so Temporal can retry / migrate workers.
type StepActivityInput struct {
	RunID           string                                `json:"run_id"`
	WorkflowID      string                                `json:"workflow_id"`
	StepID          string                                `json:"step_id"`
	PluginID        string                                `json:"plugin_id"`
	InstanceID      string                                `json:"instance_id"`
	InstanceConfig  json.RawMessage                       `json:"instance_config"`
	Action          string                                `json:"action"`
	Params          json.RawMessage                       `json:"params"`
	UpstreamOutputs map[string]map[string]json.RawMessage `json:"upstream_outputs"`
	// Resolution is the opaque blob delivered by the resolve signal when
	// this execution resumes a suspended step. Empty on first execution.
	Resolution json.RawMessage `json:"resolution,omitempty"`
}

// StepActivityResult is what RunStep returns to the Workflow function.
// When Suspended is true the plugin ended its stream with
// ExecuteSuspend: Outputs is empty and Prompt carries the opaque payload
// for the plugin's "resolution" slot.
type StepActivityResult struct {
	Outputs   map[string]json.RawMessage `json:"outputs"`
	Suspended bool                       `json:"suspended,omitempty"`
	Prompt    json.RawMessage            `json:"prompt,omitempty"`
}

// RecordStepAwaitingInput is the payload for RecordStepAwaiting, written
// to run_steps when a step suspends and starts waiting for a resolution.
type RecordStepAwaitingInput struct {
	RunID  string          `json:"run_id"`
	StepID string          `json:"step_id"`
	Prompt json.RawMessage `json:"prompt"`
}

// ResolveSignalName is the Temporal signal a suspended step waits on.
// One channel per step so concurrently-suspended steps in the same run
// are resolved independently. The signal payload is the opaque
// resolution blob (json.RawMessage).
func ResolveSignalName(stepID string) string { return "resolve-step:" + stepID }

// PersistResultInput is the payload for PersistResult, the final
// Activity each workflow runs so the orchestrator's run_summaries
// table reflects the outcome without anyone having to poll Temporal.
type PersistResultInput struct {
	Result workflow.RunResult `json:"result"`
}

// RecordRunStartInput is the payload for RecordRunStart, which opens the
// run_summaries row of a run started by a Temporal Schedule (manual runs
// get theirs from TriggerRun before the workflow starts).
type RecordRunStartInput struct {
	RunID      string    `json:"run_id"`
	WorkflowID string    `json:"workflow_id"`
	StartedAt  time.Time `json:"started_at"`
}

// RecordStepStartInput is the payload for RecordStepStart, written to
// run_steps when a step transitions from pending → running. This is what
// lets the dashboard show live per-step progress instead of waiting for
// the whole workflow to finish.
type RecordStepStartInput struct {
	RunID  string `json:"run_id"`
	StepID string `json:"step_id"`
	Plugin string `json:"plugin"`
	Action string `json:"action"`
}

// RecordStepFinishInput is the payload for RecordStepFinish, written to
// run_steps when a step reaches a terminal state (succeeded, failed, or
// skipped). Outputs are populated on success; Error on failure.
type RecordStepFinishInput struct {
	RunID   string                     `json:"run_id"`
	StepID  string                     `json:"step_id"`
	Status  string                     `json:"status"`
	Error   string                     `json:"error,omitempty"`
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`
}

// ReleaseStepInput is the payload for ReleaseStep, run once a step that
// reached its plugin at least once is terminal. Status is the terminal
// status just recorded for the step.
type ReleaseStepInput struct {
	RunID      string `json:"run_id"`
	WorkflowID string `json:"workflow_id"`
	StepID     string `json:"step_id"`
	PluginID   string `json:"plugin_id"`
	InstanceID string `json:"instance_id"`
	Action     string `json:"action"`
	Status     string `json:"status"`
}

// RecordStepProgressInput is the payload for RecordStepProgress. Called
// from RunStep as the plugin streams progress events. Payload is opaque
// bytes owned by the plugin.
type RecordStepProgressInput struct {
	RunID   string          `json:"run_id"`
	StepID  string          `json:"step_id"`
	Payload json.RawMessage `json:"payload"`
}
