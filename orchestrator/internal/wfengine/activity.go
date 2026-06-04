package wfengine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jacobsee/upstream-readiness/orchestrator/internal/registry"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/runbus"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/storage"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Activities is the receiver for the Temporal Activity functions the
// worker registers. It holds the references the activities need
// (registry for gRPC calls, store for persistence, bus for realtime
// fan-out to SSE subscribers) so they aren't global state.
type Activities struct {
	Reg   *registry.Registry
	Store *storage.Store
	Bus   *runbus.Bus
}

// RunStep is the Activity that invokes a single plugin step. All
// non-determinism (the gRPC call, retries, timeouts) lives here so the
// Workflow function can stay pure.
//
// A background heartbeat goroutine pings Temporal at half the configured
// HeartbeatTimeout while the plugin's gRPC call is in flight. The plugin
// call is opaque to us so we can't heartbeat from within it - but we can
// heartbeat around it, which is all Temporal needs to know the worker is
// still alive.
func (a *Activities) RunStep(ctx context.Context, in StepActivityInput) (StepActivityResult, error) {
	info := activity.GetInfo(ctx)
	idempotencyKey := fmt.Sprintf("%s/%s", in.RunID, in.StepID)

	upstream := make(map[string]map[string][]byte, len(in.UpstreamOutputs))
	for stepID, outs := range in.UpstreamOutputs {
		converted := make(map[string][]byte, len(outs))
		for k, v := range outs {
			converted[k] = []byte(v)
		}
		upstream[stepID] = converted
	}

	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	stopHeartbeat := startHeartbeat(ctx, info.HeartbeatTimeout, cancelCall)
	defer stopHeartbeat()

	onProgress := func(payload []byte) {
		blob := payload
		if len(blob) == 0 {
			blob = []byte(`null`)
		}
		// Persist the latest progress so a fresh SSE subscriber can
		// bootstrap without missing the current state, then fan out
		// live. Persistence errors are logged but not fatal - the live
		// event is still worth broadcasting.
		if err := a.Store.UpsertStepProgress(ctx, in.RunID, in.StepID, blob); err != nil {
			activity.GetLogger(ctx).Warn("upsert step progress", "err", err)
		}
		if a.Bus != nil {
			evt := stepProgressEvent{RunID: in.RunID, StepID: in.StepID, Payload: json.RawMessage(blob)}
			data, _ := json.Marshal(evt)
			a.Bus.Publish(in.RunID, runbus.Event{Type: "step-progress", Data: data})
		}
	}

	res, err := a.Reg.Execute(callCtx, registry.ExecuteArgs{
		PluginID:        in.PluginID,
		Action:          in.Action,
		Payload:         []byte(in.Params),
		InstanceID:      in.InstanceID,
		InstanceConfig:  []byte(in.InstanceConfig),
		WorkflowRunID:   in.RunID,
		WorkflowID:      in.WorkflowID,
		StepID:          in.StepID,
		Attempt:         uint32(info.Attempt),
		IdempotencyKey:  idempotencyKey,
		UpstreamOutputs: upstream,
		OnProgress:      onProgress,
		Resolution:      []byte(in.Resolution),
	})
	if err != nil {
		return StepActivityResult{}, classifyPluginError(err)
	}
	if res.Suspended {
		prompt := json.RawMessage(res.Prompt)
		if len(prompt) == 0 {
			prompt = json.RawMessage(`null`)
		}
		return StepActivityResult{Suspended: true, Prompt: prompt}, nil
	}

	outputs := make(map[string]json.RawMessage, len(res.Outputs))
	for k, v := range res.Outputs {
		if len(v) == 0 {
			outputs[k] = json.RawMessage(`null`)
			continue
		}
		outputs[k] = json.RawMessage(v)
	}
	return StepActivityResult{Outputs: outputs}, nil
}

// classifyPluginError marks plugin errors that retrying cannot fix as
// non-retryable. A plugin signals "deterministic failure" by returning
// gRPC FailedPrecondition or InvalidArgument; everything else (transport
// errors, Unavailable, Internal, …) keeps the activity's retry policy.
// The orchestrator still doesn't interpret the message - only the code.
func classifyPluginError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.FailedPrecondition, codes.InvalidArgument:
		return temporal.NewNonRetryableApplicationError(st.Message(), "PLUGIN_"+st.Code().String(), err)
	}
	return err
}

// PersistResult writes the final RunResult to the orchestrator's
// run_summaries table. Workflows call this as their last step so the
// HTTP API can list and inspect runs without paging Temporal history.
func (a *Activities) PersistResult(ctx context.Context, in PersistResultInput) error {
	results, err := json.Marshal(in.Result)
	if err != nil {
		return err
	}
	completed := time.Now().UTC()
	if err := a.Store.FinishRun(ctx, in.Result.RunID, in.Result.Status, completed, results); err != nil {
		return err
	}
	if a.Bus != nil {
		evt := runStatusEvent{RunID: in.Result.RunID, Status: in.Result.Status, CompletedAt: completed.Format(time.RFC3339Nano)}
		data, _ := json.Marshal(evt)
		a.Bus.Publish(in.Result.RunID, runbus.Event{Type: "run-status", Data: data})
	}
	return nil
}

// RecordRunStart opens the run_summaries row for a scheduled run, which
// has no TriggerRun to do it. Idempotent (StartRun ignores an existing
// row), so retries are safe.
func (a *Activities) RecordRunStart(ctx context.Context, in RecordRunStartInput) error {
	return a.Store.StartRun(ctx, storage.RunSummary{
		RunID: in.RunID, WorkflowID: in.WorkflowID, Status: "running", StartedAt: in.StartedAt,
	})
}

// RecordStepStart writes a run_steps row when a step begins executing.
// The workflow calls this before ExecuteActivity so the UI can see the
// step transition from pending → running.
func (a *Activities) RecordStepStart(ctx context.Context, in RecordStepStartInput) error {
	err := a.Store.UpsertStepStart(ctx, storage.StepRecord{
		RunID:     in.RunID,
		StepID:    in.StepID,
		Plugin:    in.Plugin,
		Action:    in.Action,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	})
	if err == nil && a.Bus != nil {
		evt := stepStartEvent{RunID: in.RunID, StepID: in.StepID, Plugin: in.Plugin, Action: in.Action}
		data, _ := json.Marshal(evt)
		a.Bus.Publish(in.RunID, runbus.Event{Type: "step-start", Data: data})
	}
	return err
}

// RecordStepAwaiting parks a step in awaiting_resolution with the opaque
// prompt its plugin emitted, then tells SSE subscribers so the shell can
// mount the plugin's "resolution" slot.
func (a *Activities) RecordStepAwaiting(ctx context.Context, in RecordStepAwaitingInput) error {
	err := a.Store.UpsertStepAwaiting(ctx, in.RunID, in.StepID, in.Prompt)
	if err == nil && a.Bus != nil {
		evt := stepAwaitingEvent{RunID: in.RunID, StepID: in.StepID, Prompt: in.Prompt}
		data, _ := json.Marshal(evt)
		a.Bus.Publish(in.RunID, runbus.Event{Type: "step-awaiting", Data: data})
	}
	return err
}

// RecordStepFinish writes the terminal state of a step (succeeded,
// failed, skipped) along with its outputs (on success) or error (on
// failure). The workflow calls this after each ExecuteActivity settles.
func (a *Activities) RecordStepFinish(ctx context.Context, in RecordStepFinishInput) error {
	var outputs json.RawMessage
	if len(in.Outputs) > 0 {
		b, err := json.Marshal(in.Outputs)
		if err != nil {
			return err
		}
		outputs = json.RawMessage(b)
	}
	err := a.Store.UpsertStepFinish(ctx, in.RunID, in.StepID, in.Status, in.Error, outputs, time.Now().UTC())
	if err == nil && a.Bus != nil {
		evt := stepFinishEvent{
			RunID: in.RunID, StepID: in.StepID, Status: in.Status,
			Error: in.Error, Outputs: in.Outputs,
		}
		data, _ := json.Marshal(evt)
		a.Bus.Publish(in.RunID, runbus.Event{Type: "step-finish", Data: data})
	}
	return err
}

// ReleaseStep tells the step's plugin the step is terminal (see
// Plugin.ReleaseStep) so it can drop per-step working state - above all
// the working copy of a step cancelled while suspended, which the plugin
// would otherwise keep as a resume cache.
func (a *Activities) ReleaseStep(ctx context.Context, in ReleaseStepInput) error {
	return a.Reg.ReleaseStep(ctx, registry.ReleaseStepArgs{
		PluginID:      in.PluginID,
		WorkflowRunID: in.RunID,
		StepID:        in.StepID,
		WorkflowID:    in.WorkflowID,
		InstanceID:    in.InstanceID,
		Action:        in.Action,
		Status:        in.Status,
	})
}

// stepStartEvent, stepProgressEvent, stepFinishEvent, and runStatusEvent
// are the JSON shapes the SSE stream forwards to the browser. Kept
// close to the activities that publish them so the wire shape and the
// producer stay in sync.
type stepStartEvent struct {
	RunID  string `json:"run_id"`
	StepID string `json:"step_id"`
	Plugin string `json:"plugin"`
	Action string `json:"action"`
}

type stepProgressEvent struct {
	RunID   string          `json:"run_id"`
	StepID  string          `json:"step_id"`
	Payload json.RawMessage `json:"payload"`
}

type stepAwaitingEvent struct {
	RunID  string          `json:"run_id"`
	StepID string          `json:"step_id"`
	Prompt json.RawMessage `json:"prompt"`
}

type stepFinishEvent struct {
	RunID   string                     `json:"run_id"`
	StepID  string                     `json:"step_id"`
	Status  string                     `json:"status"`
	Error   string                     `json:"error,omitempty"`
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`
}

type runStatusEvent struct {
	RunID       string `json:"run_id"`
	Status      string `json:"status"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// startHeartbeat launches a goroutine that calls activity.RecordHeartbeat
// every interval/2 (with a floor) until the returned stop func is called.
// If Temporal ever signals cancellation (e.g. the workflow was cancelled),
// the goroutine invokes cancelCall so the in-flight plugin gRPC call
// unblocks cleanly.
func startHeartbeat(ctx context.Context, interval time.Duration, cancelCall context.CancelFunc) func() {
	if interval <= 0 {
		// No heartbeat configured - nothing to do, but still watch for
		// context cancellation so we can unblock the gRPC call.
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				cancelCall()
			case <-done:
			}
		}()
		return func() { close(done) }
	}
	tick := interval / 2
	if tick < time.Second {
		tick = time.Second
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				cancelCall()
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}
