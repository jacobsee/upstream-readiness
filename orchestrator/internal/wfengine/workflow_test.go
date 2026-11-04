package wfengine

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/jacobsee/upstream-readiness/orchestrator/internal/workflow"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// TestRerunReusesStepOutputs runs a -> b -> c with a reused: a must not
// reach its plugin, b must receive a's carried-over outputs as upstream
// outputs, and every executed step must see the run inputs.
func TestRerunReusesStepOutputs(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var mu sync.Mutex
	ran := map[string]StepActivityInput{}
	env.RegisterActivityWithOptions(func(_ context.Context, in StepActivityInput) (StepActivityResult, error) {
		mu.Lock()
		ran[in.StepID] = in
		mu.Unlock()
		return StepActivityResult{Outputs: map[string]json.RawMessage{"out": json.RawMessage(`"` + in.StepID + `"`)}}, nil
	}, activity.RegisterOptions{Name: "RunStep"})
	noop := func(context.Context, any) error { return nil }
	for _, name := range []string{"PersistResult", "RecordRunStart", "RecordStepStart", "RecordStepFinish", "RecordStepAwaiting", "ReleaseStep"} {
		env.RegisterActivityWithOptions(noop, activity.RegisterOptions{Name: name})
	}

	step := func(id string, deps ...string) workflow.Step {
		return workflow.Step{ID: id, Plugin: "p", Instance: "i", Action: "run", DependsOn: deps, Params: json.RawMessage(`{}`)}
	}
	aOuts := map[string]json.RawMessage{"commit": json.RawMessage(`"abc123"`)}
	env.ExecuteWorkflow(ExecuteWorkflow, TriggerInput{
		RunID:      "wf-2",
		Definition: workflow.Definition{ID: "wf", Steps: []workflow.Step{step("a"), step("b", "a"), step("c", "b")}},
		Inputs:     map[string]string{"VERSION": "1.2"},
		Reused:     map[string]map[string]json.RawMessage{"a": aOuts},
		ReusedFrom: "wf-1",
	})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res workflow.RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}

	if _, ok := ran["a"]; ok {
		t.Error("reused step a was executed")
	}
	if got := ran["b"].UpstreamOutputs["a"]; !reflect.DeepEqual(got, aOuts) {
		t.Errorf("b upstream outputs from a = %v, want %v", got, aOuts)
	}
	for _, id := range []string{"b", "c"} {
		if ran[id].RunInputs["VERSION"] != "1.2" {
			t.Errorf("step %s run inputs = %v", id, ran[id].RunInputs)
		}
	}
	if res.Status != "succeeded" {
		t.Errorf("run status = %q, want succeeded", res.Status)
	}
	if s := res.Steps[0]; s.Status != workflow.StepReused || s.ReusedFrom != "wf-1" || !reflect.DeepEqual(s.Outputs, aOuts) {
		t.Errorf("step a result = %+v", s)
	}
	if s := res.Steps[2]; s.Status != "succeeded" {
		t.Errorf("step c result = %+v", s)
	}
}
