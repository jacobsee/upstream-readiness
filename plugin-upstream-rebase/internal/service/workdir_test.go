package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/cmdlog"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

func TestInWorkRoot(t *testing.T) {
	s := &Rebaser{WorkRoot: "/w/root"}
	for dir, want := range map[string]bool{
		"/w/root/run/step": true,
		"/w/root/run":      true,
		"/w/root":          false,
		"/w":               false,
		"/w/root/../x":     false,
		"/w/rootx/run":     false,
		"/elsewhere":       false,
		"/w/root/..run/s":  true, // a name starting with "..", still inside
	} {
		if got := s.inWorkRoot(dir); got != want {
			t.Errorf("inWorkRoot(%q) = %v, want %v", dir, got, want)
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// PurgeWorkflow removes every trace of one workflow (memory, run
// records, command history, leftover working copies) and nothing of any
// other workflow.
func TestPurgeWorkflow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	params := e.params("v1.1", e.fork, e.push)

	// A run left suspended with a working copy (as if a ReleaseStep was
	// missed), plus a saved decision: per-run and per-step state.
	run := e.workflowID + "-purge"
	if p := suspendPromptOf(t, e.exec(run, "rebase", nil, nil, params)); p.Kind != "decide" {
		t.Fatalf("suspend = %+v", p)
	}
	misc := e.table(run)["fix a thing without a prefix"]
	dec, _ := json.Marshal(map[string]any{"decisions": []map[string]string{{
		"orig_sha": misc.OrigSHA, "question": "classify", "decision": "carry", "reason": "keep",
	}}})
	e.exec(run, "decisions_save", dec, nil, nil)
	other := e.workflowID + "-other"
	if err := e.svc.Store.UpsertDecision(ctx, store.Decision{
		WorkflowID: other, StepID: "rebase", OrigSHA: misc.OrigSHA, Question: "classify", Decision: "drop", Reason: "x",
	}); err != nil {
		t.Fatal(err)
	}
	dir := e.svc.stepWorkDir(run, "rebase")
	if !exists(dir) {
		t.Fatalf("no working copy at %s", dir)
	}

	resp, err := e.svc.PurgeWorkflow(ctx, &pluginpb.PurgeWorkflowRequest{WorkflowId: e.workflowID})
	if err != nil {
		t.Fatal(err)
	}
	d := resp.GetDeleted()
	if d["runs"] != 1 || d["decisions"] != 1 || d["commits"] == 0 || d["command_log"] == 0 || d["work_dirs"] != 1 {
		t.Errorf("deleted = %v", d)
	}
	if exists(filepath.Dir(dir)) {
		t.Errorf("purge left %s behind", filepath.Dir(dir))
	}
	if r, _ := e.svc.Store.GetRun(ctx, run, "rebase"); r != nil {
		t.Errorf("run row survived: %s", r.Status)
	}
	if rows, _ := e.svc.Store.ListCommits(ctx, run, "rebase"); len(rows) != 0 {
		t.Errorf("%d commit rows survived", len(rows))
	}
	if ds, _ := e.svc.Store.ListDecisions(ctx, e.workflowID, "rebase"); len(ds) != 0 {
		t.Errorf("%d decisions survived", len(ds))
	}
	if page, err := e.svc.Store.Commands.List(ctx, run, "rebase", cmdlog.ListOptions{}); err != nil || len(page.Entries) != 0 {
		t.Errorf("command history survived: %v %v", page, err)
	}
	if ds, _ := e.svc.Store.ListDecisions(ctx, other, "rebase"); len(ds) != 1 {
		t.Errorf("another workflow's decision was deleted")
	}

	// Idempotent: a retried purge finds nothing and succeeds.
	resp, err = e.svc.PurgeWorkflow(ctx, &pluginpb.PurgeWorkflowRequest{WorkflowId: e.workflowID})
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range resp.GetDeleted() {
		if n != 0 {
			t.Errorf("second purge deleted %d %s", n, k)
		}
	}
	_, _ = e.svc.Store.Wipe(ctx, other, "", store.WipeOptions{Decisions: true})
}

// Every way a step can end must leave nothing behind in WorkRoot except
// the resume cache of a step that is still suspended.
func TestWorkDirLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	params := e.params("v1.1", e.fork, e.push)

	// 1. Suspended → working copy kept. Then the orchestrator cancels the
	//    run and releases the step → working copy and run dir gone, run
	//    record closed.
	runA := e.workflowID + "-release"
	if p := suspendPromptOf(t, e.exec(runA, "rebase", nil, nil, params)); p.Kind != "decide" {
		t.Fatalf("suspend = %+v", p)
	}
	dirA := e.svc.stepWorkDir(runA, "rebase")
	if !exists(dirA) {
		t.Fatalf("suspended run has no working copy at %s", dirA)
	}
	if _, err := e.svc.ReleaseStep(ctx, &pluginpb.ReleaseStepRequest{WorkflowRunId: runA, StepId: "rebase", Status: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Dir(dirA)) {
		t.Errorf("run dir %s survived ReleaseStep", filepath.Dir(dirA))
	}
	if run, _ := e.svc.Store.GetRun(ctx, runA, "rebase"); run.Status != "cancelled" || run.FinishedAt == nil {
		t.Errorf("released run = %s (finished %v), want cancelled", run.Status, run.FinishedAt)
	}
	// Idempotent, and fine for a step the plugin never saw.
	for _, id := range []string{runA, e.workflowID + "-never-ran"} {
		if _, err := e.svc.ReleaseStep(ctx, &pluginpb.ReleaseStepRequest{WorkflowRunId: id, StepId: "rebase", Status: "cancelled"}); err != nil {
			t.Errorf("ReleaseStep(%s): %v", id, err)
		}
	}

	// 2. Failure after cloning but before the run row is written (the
	//    previous rebase commit doesn't exist) → working copy still gone.
	var bad map[string]any
	_ = json.Unmarshal(params, &bad)
	bad["previous_rebase_commit"] = "0123456789abcdef0123456789abcdef01234567"
	badParams, _ := json.Marshal(bad)
	runB := e.workflowID + "-early-fail"
	err := e.svc.Execute(&pluginpb.ExecuteRequest{
		Action: "rebase", Payload: badParams, InstanceId: "it", InstanceConfig: e.instanceCfg,
		WorkflowRunId: runB, WorkflowId: e.workflowID, StepId: "rebase", Attempt: 1,
	}, &fakeStream{ctx: ctx})
	if err == nil {
		t.Fatal("rebase with a bogus previous_rebase_commit succeeded")
	}
	if run, _ := e.svc.Store.GetRun(ctx, runB, "rebase"); run != nil {
		t.Fatalf("expected no run row for an early failure, got %s", run.Status)
	}
	if dirB := e.svc.stepWorkDir(runB, "rebase"); exists(filepath.Dir(dirB)) {
		t.Errorf("early failure left %s behind", filepath.Dir(dirB))
	}

	// 3. Startup sweep: keeps a live suspended run's working copy, drops
	//    orphans (no row / killed mid-run) and stray files.
	runC := e.workflowID + "-live"
	suspendPromptOf(t, e.exec(runC, "rebase", nil, nil, params))
	dirC := e.svc.stepWorkDir(runC, "rebase")
	orphan := filepath.Join(e.svc.WorkRoot, "killed-run", "some-step")
	if err := os.MkdirAll(filepath.Join(orphan, "gopath", "pkg", "mod", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Read-only like the go module cache, so a plain RemoveAll would fail.
	_ = os.Chmod(filepath.Join(orphan, "gopath", "pkg", "mod"), 0o555)
	stray := filepath.Join(e.svc.WorkRoot, "stray.txt")
	_ = os.WriteFile(stray, []byte("x"), 0o600)

	if _, err := e.svc.SweepWorkRoot(ctx, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !exists(dirC) {
		t.Error("sweep removed a live suspended run's working copy")
	}
	if exists(filepath.Dir(orphan)) || exists(stray) {
		t.Error("sweep left orphans behind")
	}
	// Past the TTL even the suspended one goes.
	if _, err := e.svc.SweepWorkRoot(ctx, -time.Hour); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(e.svc.WorkRoot); len(left) != 0 {
		t.Errorf("work root not empty after expiring every run: %v", left)
	}
}
