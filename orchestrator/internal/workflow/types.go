// Package workflow defines the orchestrator's view of a workflow: a DAG
// of opaque step params plus enough metadata to identify which plugin,
// instance, and action each step targets. The orchestrator stores
// step params as raw JSON it never parses; plugins own that shape.
package workflow

import (
	"encoding/json"
	"time"
)

// Definition is the orchestrator-side shape of a workflow. Loaded from
// YAML files at startup, persisted to Postgres, executed by Temporal.
type Definition struct {
	ID          string `yaml:"id"                 json:"id"`
	DisplayName string `yaml:"display_name"       json:"display_name"`
	// Cron expression. Empty means "manual trigger only". When set, the
	// orchestrator registers a Temporal Schedule that triggers this
	// workflow at the cron cadence.
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Steps    []Step `yaml:"steps"              json:"steps"`
}

// Step is one node in the workflow DAG.
type Step struct {
	ID string `yaml:"id" json:"id"`
	// Plugin manifest ID. Must match a registered plugin at run time.
	Plugin string `yaml:"plugin" json:"plugin"`
	// Instance ID of a registered plugin instance. The plugin and the
	// instance's plugin_id must agree. Required.
	Instance string `yaml:"instance" json:"instance"`
	// Action name forwarded to Execute.
	Action string `yaml:"action" json:"action"`
	// Step IDs that must complete (successfully) before this step starts.
	DependsOn []string `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	// Opaque params blob the plugin's step-editor MFE produced (or that
	// the workflow author hand-wrote in YAML). The orchestrator never
	// parses this. May contain marker objects only the plugin understands
	// for binding fields to upstream step outputs.
	Params json.RawMessage `yaml:"params,omitempty" json:"params,omitempty"`
	// Timeout bounds a single execution of this step's plugin call, as a
	// Go duration string (e.g. "90m", "4h"). Empty means
	// DefaultStepTimeout. Time a step spends awaiting_resolution does not
	// count - the wait happens in the Temporal workflow, between
	// executions. Orchestrator-owned; never forwarded to the plugin.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// DefaultStepTimeout is the per-execution StartToClose timeout applied
// when a step doesn't set Timeout.
const DefaultStepTimeout = 30 * time.Minute

// StepTimeout returns the parsed Timeout, or DefaultStepTimeout when
// unset. Callers should have run ValidateShape first; an unparseable
// value also falls back to the default.
func (s Step) StepTimeout() time.Duration {
	if s.Timeout == "" {
		return DefaultStepTimeout
	}
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 {
		return DefaultStepTimeout
	}
	return d
}

// StepResult is the per-step record the Temporal workflow returns to
// the orchestrator's run summary. Outputs are the structured map the
// plugin emitted; downstream steps received these as upstream_outputs.
type StepResult struct {
	StepID  string                     `json:"step_id"`
	Plugin  string                     `json:"plugin"`
	Action  string                     `json:"action"`
	Status  string                     `json:"status"` // pending | running | awaiting_resolution | succeeded | failed | skipped | cancelled
	Error   string                     `json:"error,omitempty"`
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`
}

// RunResult is the JSON shape the orchestrator's HTTP API returns for a
// completed run. It mirrors what gets persisted in run_summaries.results.
type RunResult struct {
	RunID      string       `json:"run_id"`
	WorkflowID string       `json:"workflow_id"`
	Status     string       `json:"status"` // running | succeeded | failed
	Steps      []StepResult `json:"steps"`
}
