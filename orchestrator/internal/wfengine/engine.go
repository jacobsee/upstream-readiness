package wfengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jacobsee/upstream-readiness/orchestrator/internal/registry"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/runbus"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/storage"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/workflow"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Engine bundles the Temporal client and the worker that consumes the
// workflow task queue. One Engine per orchestrator process.
type Engine struct {
	client    client.Client
	worker    worker.Worker
	store     *storage.Store
	reg       *registry.Registry
	bus       *runbus.Bus
	namespace string
}

// Start dials Temporal, registers the workflow + activities, and
// starts the worker goroutine. Call Stop on shutdown.
func Start(ctx context.Context, address string, reg *registry.Registry, store *storage.Store, bus *runbus.Bus) (*Engine, error) {
	c, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return nil, fmt.Errorf("temporal dial %s: %w", address, err)
	}
	w := worker.New(c, TaskQueue, worker.Options{})
	acts := &Activities{Reg: reg, Store: store, Bus: bus}
	w.RegisterWorkflow(ExecuteWorkflow)
	w.RegisterActivityWithOptions(acts.RunStep, activity.RegisterOptions{Name: "RunStep"})
	w.RegisterActivityWithOptions(acts.PersistResult, activity.RegisterOptions{Name: "PersistResult"})
	w.RegisterActivityWithOptions(acts.RecordRunStart, activity.RegisterOptions{Name: "RecordRunStart"})
	w.RegisterActivityWithOptions(acts.RecordStepStart, activity.RegisterOptions{Name: "RecordStepStart"})
	w.RegisterActivityWithOptions(acts.RecordStepFinish, activity.RegisterOptions{Name: "RecordStepFinish"})
	w.RegisterActivityWithOptions(acts.RecordStepAwaiting, activity.RegisterOptions{Name: "RecordStepAwaiting"})
	w.RegisterActivityWithOptions(acts.ReleaseStep, activity.RegisterOptions{Name: "ReleaseStep"})
	if err := w.Start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("worker start: %w", err)
	}
	return &Engine{client: c, worker: w, store: store, reg: reg, bus: bus, namespace: client.DefaultNamespace}, nil
}

// Bus exposes the run-event bus so the HTTP layer can attach SSE
// subscribers to it.
func (e *Engine) Bus() *runbus.Bus { return e.bus }

// Stop tears down the worker and closes the Temporal client.
func (e *Engine) Stop() {
	if e.worker != nil {
		e.worker.Stop()
	}
	if e.client != nil {
		e.client.Close()
	}
}

// TriggerRun starts a workflow execution. It allocates a run ID, loads
// the instance configs the workflow references from Postgres, opens a
// run_summaries row in "running" status, and schedules the Temporal
// workflow. The Temporal workflow's PersistResult activity closes the
// summary when it finishes.
func (e *Engine) TriggerRun(ctx context.Context, def workflow.Definition) (string, error) {
	if err := workflow.ValidateShape(def); err != nil {
		return "", err
	}
	instanceIDs := collectInstanceIDs(def)
	configs := make(map[string]json.RawMessage, len(instanceIDs))
	for _, id := range instanceIDs {
		inst, err := e.store.GetInstance(ctx, id)
		if err != nil {
			return "", fmt.Errorf("load instance %q: %w", id, err)
		}
		if inst == nil {
			return "", fmt.Errorf("workflow references unknown instance %q", id)
		}
		if inst.PluginID == "" {
			return "", fmt.Errorf("instance %q has no plugin_id", id)
		}
		configs[id] = inst.ConfigBlob
	}

	runID := fmt.Sprintf("%s-%d", def.ID, time.Now().UnixMilli())
	if err := e.store.StartRun(ctx, storage.RunSummary{
		RunID:      runID,
		WorkflowID: def.ID,
		Status:     "running",
		StartedAt:  time.Now().UTC(),
	}); err != nil {
		return "", fmt.Errorf("start run summary: %w", err)
	}

	wfOpts := client.StartWorkflowOptions{
		ID:        runID,
		TaskQueue: TaskQueue,
	}
	if _, err := e.client.ExecuteWorkflow(ctx, wfOpts, ExecuteWorkflow, TriggerInput{
		RunID:           runID,
		Definition:      def,
		InstanceConfigs: configs,
	}); err != nil {
		// Mark the summary failed so the UI doesn't show a ghost
		// "running" forever.
		_ = e.store.FinishRun(ctx, runID, "failed", time.Now().UTC(), mustJSON(map[string]string{"error": err.Error()}))
		return "", fmt.Errorf("start workflow: %w", err)
	}
	return runID, nil
}

// EnsureSchedules creates or updates a Temporal Schedule for every
// workflow whose Schedule field is non-empty. Workflows without a
// schedule are left untouched. Existing Schedules whose cron expression
// changed are recreated; ones whose workflow definition was removed
// are deleted.
func (e *Engine) EnsureSchedules(ctx context.Context, defs []workflow.Definition) error {
	want := map[string]workflow.Definition{}
	for _, d := range defs {
		if d.Schedule != "" {
			want[scheduleID(d.ID)] = d
		}
	}

	listing, err := e.client.ScheduleClient().List(ctx, client.ScheduleListOptions{})
	if err != nil {
		return fmt.Errorf("list schedules: %w", err)
	}
	have := map[string]struct{}{}
	for listing.HasNext() {
		entry, err := listing.Next()
		if err != nil {
			return fmt.Errorf("list schedules: %w", err)
		}
		if isOwnedSchedule(entry.ID) {
			have[entry.ID] = struct{}{}
		}
	}

	for id := range have {
		if _, keep := want[id]; !keep {
			h := e.client.ScheduleClient().GetHandle(ctx, id)
			if err := h.Delete(ctx); err != nil {
				return fmt.Errorf("delete schedule %q: %w", id, err)
			}
		}
	}

	for id, def := range want {
		instanceIDs := collectInstanceIDs(def)
		configs := make(map[string]json.RawMessage, len(instanceIDs))
		for _, instID := range instanceIDs {
			inst, err := e.store.GetInstance(ctx, instID)
			if err != nil {
				return fmt.Errorf("load instance %q for schedule: %w", instID, err)
			}
			if inst == nil {
				return fmt.Errorf("scheduled workflow %q references unknown instance %q", def.ID, instID)
			}
			configs[instID] = inst.ConfigBlob
		}
		input := TriggerInput{
			RunID:           "", // ExecuteWorkflow uses the fire's workflow ID
			Definition:      def,
			InstanceConfigs: configs,
		}
		action := &client.ScheduleWorkflowAction{
			// Temporal appends the scheduled time ("-2026-10-08T06:00:00Z"),
			// so fires get run IDs shaped like TriggerRun's "<id>-<ms>".
			ID:        def.ID,
			Workflow:  ExecuteWorkflow,
			TaskQueue: TaskQueue,
			Args:      []any{input},
		}
		spec := client.ScheduleSpec{CronExpressions: []string{def.Schedule}}
		opts := client.ScheduleOptions{
			ID:     id,
			Spec:   spec,
			Action: action,
		}
		_, exists := have[id]
		if exists {
			h := e.client.ScheduleClient().GetHandle(ctx, id)
			if err := h.Delete(ctx); err != nil {
				return fmt.Errorf("recreate schedule %q (delete): %w", id, err)
			}
		}
		if _, err := e.client.ScheduleClient().Create(ctx, opts); err != nil {
			return fmt.Errorf("create schedule %q: %w", id, err)
		}
	}
	return nil
}

// CancelRun asks Temporal to cancel the workflow whose ID matches runID.
// Temporal delivers a cancellation to any in-flight activity's context;
// RunStep's heartbeat goroutine watches for that and cancels the plugin
// gRPC call, unwinding cleanly. The run summary is marked "failed" only
// once PersistResult (or the reconciliation in GetRunStatus) sees the
// terminal state - we don't force the summary row here to avoid diverging
// from Temporal's authoritative view.
func (e *Engine) CancelRun(ctx context.Context, runID string) error {
	summary, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("unknown run %q", runID)
	}
	if summary.Status != "running" {
		return fmt.Errorf("run %q is not running (status=%s)", runID, summary.Status)
	}
	return e.client.CancelWorkflow(ctx, runID, "")
}

// ErrPurgeBlocked is returned by PurgeWorkflow when the purge can't
// start yet (runs still in flight, a plugin the workflow used is not
// registered). Nothing has been deleted.
var ErrPurgeBlocked = errors.New("purge blocked")

// PurgeReport says what a purge deleted. Plugins maps plugin ID to that
// plugin's own per-category counts.
type PurgeReport struct {
	Plugins      map[string]map[string]int64 `json:"plugins"`
	TemporalRuns int                         `json:"temporal_runs"`
	Orchestrator map[string]int64            `json:"orchestrator"`
	PluginErrors map[string]string           `json:"plugin_errors,omitempty"`
}

// PurgeWorkflow deletes every trace of a removed workflow: first each
// plugin's state for it (Plugin.PurgeWorkflow), then the Temporal
// executions of its runs and its schedule, then the orchestrator's own
// rows. Plugins go first so a plugin failure leaves the workflow listed
// as removed and the purge can simply be retried; every step is
// idempotent. Refuses while any of the workflow's runs is in flight.
func (e *Engine) PurgeWorkflow(ctx context.Context, workflowID string) (*PurgeReport, error) {
	rec, err := e.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.RemovedAt == nil {
		return nil, storage.ErrWorkflowNotRemoved
	}

	runs, err := e.store.ListAllRunsForWorkflow(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, r := range runs {
		if r.Status != "running" {
			continue
		}
		// A "running" summary may be stale (PersistResult lost);
		// GetRunStatus reconciles it with Temporal.
		s, err := e.GetRunStatus(ctx, r.RunID)
		if err != nil {
			return nil, err
		}
		if s != nil && s.Status == "running" {
			live = append(live, r.RunID)
		}
	}
	if len(live) > 0 {
		return nil, fmt.Errorf("%w: cancel these runs first: %s", ErrPurgeBlocked, strings.Join(live, ", "))
	}

	// Every registered plugin is asked, so state left by plugins only an
	// older version of the definition used is caught too. A plugin the
	// workflow is known to have used but that isn't registered right now
	// would silently keep its state, so that blocks the purge.
	registered := map[string]bool{}
	for _, m := range e.reg.Manifests() {
		registered[m.GetId()] = true
	}
	used, err := e.store.PluginsUsedByWorkflow(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	var def workflow.Definition
	if err := json.Unmarshal(rec.Definition, &def); err == nil {
		for _, s := range def.Steps {
			used = append(used, s.Plugin)
		}
	}
	var missing []string
	seen := map[string]bool{}
	for _, p := range used {
		if p != "" && !registered[p] && !seen[p] {
			seen[p] = true
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%w: plugin(s) %s used by this workflow are not registered; start them and restart the orchestrator so their state is purged too",
			ErrPurgeBlocked, strings.Join(missing, ", "))
	}

	report := &PurgeReport{Plugins: map[string]map[string]int64{}, PluginErrors: map[string]string{}}
	for id := range registered {
		deleted, err := e.reg.PurgeWorkflow(ctx, id, workflowID)
		if err != nil {
			report.PluginErrors[id] = err.Error()
			continue
		}
		if len(deleted) > 0 {
			report.Plugins[id] = deleted
		}
	}
	if len(report.PluginErrors) > 0 {
		return report, fmt.Errorf("plugin purge failed; nothing else was deleted, retry the purge")
	}

	ns := e.client.WorkflowService()
	for _, r := range runs {
		_, err := ns.DeleteWorkflowExecution(ctx, &workflowservice.DeleteWorkflowExecutionRequest{
			Namespace:         e.namespace,
			WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: r.RunID},
		})
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			// Never started (TriggerRun failed), past retention, or
			// deleted by an earlier attempt.
			continue
		}
		if err != nil {
			return report, fmt.Errorf("delete Temporal execution %q: %w", r.RunID, err)
		}
		report.TemporalRuns++
	}
	if err := e.client.ScheduleClient().GetHandle(ctx, scheduleID(workflowID)).Delete(ctx); err != nil {
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			return report, fmt.Errorf("delete schedule: %w", err)
		}
	}

	report.Orchestrator, err = e.store.PurgeWorkflow(ctx, workflowID)
	if err != nil {
		return report, err
	}
	report.PluginErrors = nil
	return report, nil
}

// ErrStepNotAwaiting is returned by ResolveStep when the step isn't
// currently parked in awaiting_resolution.
var ErrStepNotAwaiting = errors.New("step is not awaiting resolution")

// ResolveStep delivers an opaque resolution blob to a suspended step by
// signalling its Temporal workflow. The workflow re-executes the step
// with ExecuteRequest.resolution = blob. The orchestrator never parses
// the blob.
func (e *Engine) ResolveStep(ctx context.Context, runID, stepID string, blob json.RawMessage) error {
	st, err := e.store.GetStepStatus(ctx, runID, stepID)
	if err != nil {
		return err
	}
	if st != "awaiting_resolution" {
		return fmt.Errorf("%w (run %q step %q status=%q)", ErrStepNotAwaiting, runID, stepID, st)
	}
	if len(blob) == 0 {
		blob = json.RawMessage(`null`)
	}
	return e.client.SignalWorkflow(ctx, runID, "", ResolveSignalName(stepID), blob)
}

// GetRunStatus combines the Postgres summary with Temporal's live view.
// If the summary says "running" but Temporal reports a terminal state,
// the summary is stale and we trust Temporal - this can happen when
// PersistResult itself fails or is in flight.
//
// For runs that have not yet reached PersistResult, results is populated
// on the fly from the run_steps lifecycle table so the dashboard can
// render live per-step progress instead of an empty pending graph.
func (e *Engine) GetRunStatus(ctx context.Context, runID string) (*storage.RunSummary, error) {
	summary, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, nil
	}

	// If PersistResult hasn't landed yet, overlay live per-step state.
	if len(summary.Results) == 0 || string(summary.Results) == "null" {
		steps, err := e.store.ListStepsForRun(ctx, runID)
		if err == nil && len(steps) > 0 {
			live := workflow.RunResult{
				RunID:      summary.RunID,
				WorkflowID: summary.WorkflowID,
				Status:     summary.Status,
				Steps:      make([]workflow.StepResult, 0, len(steps)),
			}
			for _, s := range steps {
				sr := workflow.StepResult{
					StepID: s.StepID, Plugin: s.Plugin, Action: s.Action,
					Status: s.Status, Error: s.Error,
				}
				if len(s.Outputs) > 0 {
					var outs map[string]json.RawMessage
					if err := json.Unmarshal(s.Outputs, &outs); err == nil {
						sr.Outputs = outs
					}
				}
				live.Steps = append(live.Steps, sr)
			}
			if b, err := json.Marshal(live); err == nil {
				summary.Results = json.RawMessage(b)
			}
		}
	}

	if summary.Status != "running" {
		return summary, nil
	}
	desc, err := e.client.DescribeWorkflowExecution(ctx, runID, "")
	if err != nil {
		// Surface the summary as-is; Temporal may be unreachable.
		return summary, nil
	}
	st := desc.GetWorkflowExecutionInfo().GetStatus()
	newStatus := ""
	switch st.String() {
	case "Completed":
		newStatus = "succeeded"
	case "Failed", "Terminated", "TimedOut", "Canceled":
		newStatus = "failed"
	}
	if newStatus != "" {
		summary.Status = newStatus
		// Also persist the reconciled status so ListRuns and the runs
		// dashboard don't keep displaying "running" for a terminated run.
		// If PersistResult is still in flight this is a no-op update; if
		// it never ran (Temporal terminated the workflow externally) this
		// is the only place the summary gets closed.
		completed := time.Now().UTC()
		if info := desc.GetWorkflowExecutionInfo(); info != nil && info.GetCloseTime() != nil {
			completed = info.GetCloseTime().AsTime()
		}
		_ = e.store.FinishRun(ctx, runID, newStatus, completed, summary.Results)
		summary.CompletedAt = &completed
	}
	return summary, nil
}

func collectInstanceIDs(def workflow.Definition) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range def.Steps {
		if s.Instance == "" || seen[s.Instance] {
			continue
		}
		seen[s.Instance] = true
		out = append(out, s.Instance)
	}
	return out
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// schedulePrefix is the marker that lets us recognise schedules this
// orchestrator created, so we don't accidentally delete a schedule
// owned by something else sharing the Temporal namespace.
const schedulePrefix = "upstream-readiness:"

func scheduleID(workflowID string) string { return schedulePrefix + workflowID }
func isOwnedSchedule(id string) bool {
	return len(id) > len(schedulePrefix) && id[:len(schedulePrefix)] == schedulePrefix
}
