package wfengine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jacobsee/upstream-readiness/orchestrator/internal/workflow"
	"go.temporal.io/sdk/temporal"
	tworkflow "go.temporal.io/sdk/workflow"
)

// ExecuteWorkflow is the Temporal Workflow function that drives a
// workflow run. It walks the DAG, dispatches each step as a RunStep
// Activity, threads upstream outputs forward, and writes the final
// summary back to Postgres via PersistResult.
//
// The function must remain deterministic - no goroutines, no time.Now,
// no map iteration with side effects on order. We use Temporal's
// workflow primitives (Selector, Future) for concurrency.
func ExecuteWorkflow(ctx tworkflow.Context, in TriggerInput) (workflow.RunResult, error) {
	logger := tworkflow.GetLogger(ctx)
	logger.Info("workflow started", "run_id", in.RunID, "workflow_id", in.Definition.ID)

	if err := workflow.ValidateShape(in.Definition); err != nil {
		return workflow.RunResult{
			RunID: in.RunID, WorkflowID: in.Definition.ID, Status: "failed",
		}, temporal.NewNonRetryableApplicationError("invalid workflow shape", "WORKFLOW_INVALID", err)
	}

	// A Temporal Schedule fire has no TriggerRun in front of it: its input
	// carries no RunID and nothing opened a run_summaries row. Use the
	// workflow ID the schedule generated (as TriggerRun does for manual
	// runs) and open the row here.
	if in.RunID == "" {
		in.RunID = tworkflow.GetInfo(ctx).WorkflowExecution.ID
		startCtx := tworkflow.WithActivityOptions(ctx, tworkflow.ActivityOptions{
			StartToCloseTimeout: 30 * time.Second,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:    time.Second,
				BackoffCoefficient: 2.0,
				MaximumInterval:    30 * time.Second,
				MaximumAttempts:    10,
			},
		})
		if err := tworkflow.ExecuteActivity(startCtx, "RecordRunStart", RecordRunStartInput{
			RunID: in.RunID, WorkflowID: in.Definition.ID, StartedAt: tworkflow.Now(ctx).UTC(),
		}).Get(startCtx, nil); err != nil {
			logger.Error("record scheduled run start failed", "err", err)
		}
	}

	// Per-step coordination state. We use Temporal's Future/Settable
	// primitives so that "waiting on a dependency" is a workflow-level
	// wait, not a Go channel (which workflows must not use).
	type stepState struct {
		done    tworkflow.Future
		settle  tworkflow.Settable
		result  StepActivityResult
		failed  bool
		skipped bool
		errMsg  string
	}
	states := make(map[string]*stepState, len(in.Definition.Steps))
	for _, s := range in.Definition.Steps {
		f, set := tworkflow.NewFuture(ctx)
		states[s.ID] = &stepState{done: f, settle: set}
	}

	// Step results assembled in declaration order so the final summary
	// is stable for the UI.
	stepIdx := make(map[string]int, len(in.Definition.Steps))
	for i, s := range in.Definition.Steps {
		stepIdx[s.ID] = i
	}
	stepResults := make([]workflow.StepResult, len(in.Definition.Steps))
	for i, s := range in.Definition.Steps {
		stepResults[i] = workflow.StepResult{
			StepID: s.ID, Plugin: s.Plugin, Action: s.Action, Status: "pending",
		}
	}

	activityOpts := tworkflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    2 * time.Minute,
			MaximumAttempts:    3,
		},
	}
	stepCtx := tworkflow.WithActivityOptions(ctx, activityOpts)

	// Best-effort lifecycle activities so the UI can render live per-step
	// progress without paging Temporal history. Short timeouts / bounded
	// retries - a lost record just means a stale UI, not a failed run.
	// These options are applied to each per-step goroutine's context; you
	// cannot share a workflow-level ActivityOptions ctx across goroutines
	// because Temporal enforces one blocking Future per coroutine.
	recordOpts := tworkflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    500 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    5 * time.Second,
			MaximumAttempts:    5,
		},
	}

	// ReleaseStep tells a plugin a step it ran is terminal so it can drop
	// per-step working state (e.g. the working copy of a step cancelled
	// while suspended). Best-effort: a failure leaves the plugin to its
	// own startup sweep, never fails the run. Generous timeout because
	// releasing can mean deleting a multi-GB clone + module cache.
	releaseOpts := tworkflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
	// Step goroutines settle their future before releasing, so the main
	// goroutine waits on this too before returning; otherwise in-flight
	// ReleaseStep activities would be abandoned when the workflow ends.
	stepsDone := tworkflow.NewWaitGroup(ctx)
	stepsDone.Add(len(in.Definition.Steps))

	for i := range in.Definition.Steps {
		step := in.Definition.Steps[i]
		state := states[step.ID]
		tworkflow.Go(stepCtx, func(gctx tworkflow.Context) {
			defer stepsDone.Done()
			recordCtx := tworkflow.WithActivityOptions(gctx, recordOpts)
			// Terminal writes go through a disconnected ctx derived from
			// this goroutine's own gctx so a workflow cancellation
			// mid-step still gets a terminal row written for the step
			// that was in flight. Deriving from gctx (not from the
			// workflow's main ctx) is critical: Temporal ties coroutine
			// state to the ctx, and using a foreign ctx inside another
			// goroutine panics.
			gDisconn, _ := tworkflow.NewDisconnectedContext(gctx)
			finishCtx := tworkflow.WithActivityOptions(gDisconn, recordOpts)
			releaseCtx := tworkflow.WithActivityOptions(gDisconn, releaseOpts)
			release := func(status string) {
				_ = tworkflow.ExecuteActivity(releaseCtx, "ReleaseStep", ReleaseStepInput{
					RunID: in.RunID, WorkflowID: in.Definition.ID, StepID: step.ID, PluginID: step.Plugin,
					InstanceID: step.Instance, Action: step.Action, Status: status,
				}).Get(releaseCtx, nil)
			}

			// Wait for every declared dependency. If any failed/skipped,
			// mark this step skipped and signal the future.
			for _, depID := range step.DependsOn {
				dep := states[depID]
				if err := dep.done.Get(gctx, nil); err != nil {
					// Future itself never errors; we only care about the
					// dep's failure flag set by the producer goroutine.
					_ = err
				}
				if dep.failed || dep.skipped {
					state.skipped = true
					state.errMsg = fmt.Sprintf("skipped: dependency %q did not succeed", depID)
					stepResults[stepIdx[step.ID]].Status = "skipped"
					stepResults[stepIdx[step.ID]].Error = state.errMsg
					_ = tworkflow.ExecuteActivity(finishCtx, "RecordStepFinish", RecordStepFinishInput{
						RunID: in.RunID, StepID: step.ID, Status: "skipped", Error: state.errMsg,
					}).Get(finishCtx, nil)
					state.settle.Set(nil, nil)
					return
				}
			}

			// Build the upstream outputs map from completed dependencies.
			upstream := make(map[string]map[string]json.RawMessage)
			for _, depID := range step.DependsOn {
				if outs := states[depID].result.Outputs; len(outs) > 0 {
					upstream[depID] = outs
				}
			}

			instanceConfig := in.InstanceConfigs[step.Instance]
			if len(instanceConfig) == 0 {
				instanceConfig = json.RawMessage(`{}`)
			}

			actInput := StepActivityInput{
				RunID:           in.RunID,
				WorkflowID:      in.Definition.ID,
				StepID:          step.ID,
				PluginID:        step.Plugin,
				InstanceID:      step.Instance,
				InstanceConfig:  instanceConfig,
				Action:          step.Action,
				Params:          step.Params,
				UpstreamOutputs: upstream,
			}

			// Per-step StartToClose: steps that legitimately run long (a
			// large clone + rebase + make) set `timeout:` in the workflow
			// definition. Heartbeat + retry policy stay shared.
			stepOpts := activityOpts
			stepOpts.StartToCloseTimeout = step.StepTimeout()
			runCtx := tworkflow.WithActivityOptions(gctx, stepOpts)
			resolveCh := tworkflow.GetSignalChannel(gctx, ResolveSignalName(step.ID))

			// Execute → (suspend → wait for resolve signal → re-execute)*.
			// The wait happens here in the workflow, not inside an
			// activity, so it has no timeout and survives orchestrator
			// restarts. Each re-execution is a fresh RunStep with the
			// opaque resolution blob attached.
			var actResult StepActivityResult
			var err error
			for {
				_ = tworkflow.ExecuteActivity(recordCtx, "RecordStepStart", RecordStepStartInput{
					RunID: in.RunID, StepID: step.ID, Plugin: step.Plugin, Action: step.Action,
				}).Get(recordCtx, nil)
				stepResults[stepIdx[step.ID]].Status = "running"

				actResult = StepActivityResult{}
				err = tworkflow.ExecuteActivity(runCtx, "RunStep", actInput).Get(runCtx, &actResult)
				if err != nil || !actResult.Suspended {
					break
				}

				// Drop any resolve signal that arrived before this
				// suspension was recorded (double-submits, races with a
				// previous prompt) so a stale blob can't answer a new
				// prompt. Signals sent after RecordStepAwaiting lands are
				// the legitimate ones and are received below.
				var stale json.RawMessage
				for resolveCh.ReceiveAsync(&stale) {
				}
				stepResults[stepIdx[step.ID]].Status = "awaiting_resolution"
				_ = tworkflow.ExecuteActivity(recordCtx, "RecordStepAwaiting", RecordStepAwaitingInput{
					RunID: in.RunID, StepID: step.ID, Prompt: actResult.Prompt,
				}).Get(recordCtx, nil)

				var blob json.RawMessage
				sel := tworkflow.NewSelector(gctx)
				sel.AddReceive(resolveCh, func(c tworkflow.ReceiveChannel, _ bool) {
					c.Receive(gctx, &blob)
				})
				sel.AddReceive(gctx.Done(), func(tworkflow.ReceiveChannel, bool) {})
				sel.Select(gctx)
				if gctx.Err() != nil {
					err = temporal.NewCanceledError("cancelled while awaiting resolution")
					break
				}
				actInput.Resolution = blob
			}
			if err != nil {
				// Distinguish cancellation from other failures so the UI
				// reports "cancelled" rather than a raw retry-exhausted error.
				status := "failed"
				if gctx.Err() != nil || temporal.IsCanceledError(err) {
					status = "cancelled"
					state.errMsg = "cancelled"
				} else {
					state.errMsg = err.Error()
				}
				state.failed = true
				stepResults[stepIdx[step.ID]].Status = status
				stepResults[stepIdx[step.ID]].Error = state.errMsg
				_ = tworkflow.ExecuteActivity(finishCtx, "RecordStepFinish", RecordStepFinishInput{
					RunID: in.RunID, StepID: step.ID, Status: status, Error: state.errMsg,
				}).Get(finishCtx, nil)
				// Settle before releasing so dependents aren't held up by
				// the plugin's cleanup.
				state.settle.Set(nil, nil)
				release(status)
				return
			}
			state.result = actResult
			stepResults[stepIdx[step.ID]].Status = "succeeded"
			stepResults[stepIdx[step.ID]].Outputs = actResult.Outputs
			_ = tworkflow.ExecuteActivity(finishCtx, "RecordStepFinish", RecordStepFinishInput{
				RunID: in.RunID, StepID: step.ID, Status: "succeeded", Outputs: actResult.Outputs,
			}).Get(finishCtx, nil)
			state.settle.Set(nil, nil)
			release("succeeded")
		})
	}

	for _, s := range in.Definition.Steps {
		_ = states[s.ID].done.Get(ctx, nil)
	}

	// Disconnected ctx used for the main-goroutine cleanup + PersistResult
	// so both survive workflow cancellation.
	mainDisconn, _ := tworkflow.NewDisconnectedContext(ctx)
	mainFinishCtx := tworkflow.WithActivityOptions(mainDisconn, recordOpts)

	// If the workflow was cancelled while some steps were still pending
	// (their goroutines blocked on a dependency or on RunStep), those
	// steps still show "pending" in stepResults and may have no run_steps
	// row. Convert them into terminal "cancelled" entries so the UI stops
	// showing them as pending forever.
	if ctx.Err() != nil {
		for i, sr := range stepResults {
			if sr.Status == "pending" || sr.Status == "running" || sr.Status == "awaiting_resolution" {
				stepResults[i].Status = "cancelled"
				stepResults[i].Error = "cancelled"
				_ = tworkflow.ExecuteActivity(mainFinishCtx, "RecordStepFinish", RecordStepFinishInput{
					RunID: in.RunID, StepID: sr.StepID, Status: "cancelled", Error: "cancelled",
				}).Get(mainFinishCtx, nil)
			}
		}
	}

	status := "succeeded"
	for _, sr := range stepResults {
		if sr.Status == "failed" || sr.Status == "skipped" || sr.Status == "cancelled" {
			status = "failed"
			break
		}
	}

	result := workflow.RunResult{
		RunID:      in.RunID,
		WorkflowID: in.Definition.ID,
		Status:     status,
		Steps:      stepResults,
	}

	// Persist the final summary. Use a longer retry policy here because
	// losing this write means the orchestrator's run history would
	// diverge from Temporal's record. Use a disconnected context so this
	// activity still runs when the workflow was cancelled - otherwise the
	// run_summaries row would stay stuck at "running" until GetRunStatus
	// reconciled with Temporal on the next UI poll.
	persistCtx := tworkflow.WithActivityOptions(mainDisconn, tworkflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    10,
		},
	})
	persistErr := tworkflow.ExecuteActivity(persistCtx, "PersistResult", PersistResultInput{Result: result}).Get(persistCtx, nil)
	// The run's outcome is already recorded; now let the plugins finish
	// releasing step state before the workflow closes.
	stepsDone.Wait(mainDisconn)
	if persistErr != nil {
		logger.Error("persist result failed", "err", persistErr)
		return result, persistErr
	}
	// If the workflow was cancelled, surface that back to Temporal so
	// the workflow ends in the Canceled state (not Completed). This
	// keeps Temporal's authoritative view in agreement with the "failed"
	// status we just persisted.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}
