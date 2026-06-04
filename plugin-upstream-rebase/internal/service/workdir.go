package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobsee/upstream-readiness/pluginkit/gitenv"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Working copies live at <WorkRoot>/<run>/<step>. One is kept only while
// its step is suspended in the orchestrator; every other way a step can
// end drops it:
//
//   - succeeded / failed inside Execute → finish / fail remove it;
//   - cancelled (or otherwise finished) in the orchestrator while
//     suspended → the orchestrator calls ReleaseStep;
//   - plugin killed mid-run, or a ReleaseStep that never arrived →
//     SweepWorkRoot at the next startup.

// stepWorkDir is where the (run, step) working copy lives.
func (s *Rebaser) stepWorkDir(runID, stepID string) string {
	return filepath.Join(s.WorkRoot, safeName(runID), safeName(stepID))
}

// removeWorkDir deletes a step's working copy and then its run dir if
// that is now empty (other steps of the run may still hold theirs).
func (s *Rebaser) removeWorkDir(dir string) error {
	if dir == "" || !s.inWorkRoot(dir) {
		return nil
	}
	if err := gitenv.RemoveAllWritable(dir); err != nil {
		return err
	}
	// os.Remove refuses a non-empty dir, which is exactly the check we want.
	_ = os.Remove(filepath.Dir(dir))
	return nil
}

// inWorkRoot reports whether dir is strictly inside WorkRoot. Every
// delete goes through it, so a bad DB row can never point us elsewhere.
func (s *Rebaser) inWorkRoot(dir string) bool {
	rel, err := filepath.Rel(s.WorkRoot, dir)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ReleaseStep is the orchestrator saying the step is terminal and will
// never be resumed. Drop its working copy and close out a run record
// still marked running / suspended. Saved resolutions, decisions, rerere
// entries, and the patch queue are per-(workflow, step) memory and stay.
func (s *Rebaser) ReleaseStep(ctx context.Context, req *pluginpb.ReleaseStepRequest) (*pluginpb.ReleaseStepResponse, error) {
	runID, stepID := req.GetWorkflowRunId(), req.GetStepId()
	if runID == "" || stepID == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow_run_id and step_id are required")
	}
	if s.Store != nil {
		if err := s.Store.CloseOpenRun(ctx, runID, stepID,
			fmt.Sprintf("step ended in the orchestrator (%s) before the rebase finished", req.GetStatus())); err != nil {
			return nil, status.Errorf(codes.Internal, "close run record: %v", err)
		}
	}
	if err := s.removeWorkDir(s.stepWorkDir(runID, stepID)); err != nil {
		return nil, status.Errorf(codes.Internal, "remove work dir: %v", err)
	}
	return &pluginpb.ReleaseStepResponse{}, nil
}

// PurgeWorkflow deletes everything kept for a workflow the orchestrator
// is purging. Working copies go first (any a missed ReleaseStep left),
// so if the DB delete then fails, a retry still finds the runs and
// nothing is orphaned on disk.
func (s *Rebaser) PurgeWorkflow(ctx context.Context, req *pluginpb.PurgeWorkflowRequest) (*pluginpb.PurgeWorkflowResponse, error) {
	wf := req.GetWorkflowId()
	if wf == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow_id is required")
	}
	if s.Store == nil {
		return nil, status.Error(codes.FailedPrecondition, "plugin store is not configured; set --pg-dsn or PG_DSN")
	}
	keys, err := s.Store.ListRunKeys(ctx, wf)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list runs: %v", err)
	}
	var dirs int64
	for _, k := range keys {
		dir := s.stepWorkDir(k.WorkflowRunID, k.StepID)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := s.removeWorkDir(dir); err != nil {
			return nil, status.Errorf(codes.Internal, "remove work dir: %v", err)
		}
		dirs++
	}
	counts, err := s.Store.PurgeWorkflow(ctx, wf)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	counts["work_dirs"] = dirs
	return &pluginpb.PurgeWorkflowResponse{Deleted: counts}, nil
}

// SweepWorkRoot deletes everything under WorkRoot except the working
// copies of runs still suspended and touched within ttl. Must run before
// the gRPC server starts: any other dir belongs to a job that is no
// longer running (this process hasn't started any yet), so it can only
// be an orphan. Returns the paths it removed.
func (s *Rebaser) SweepWorkRoot(ctx context.Context, ttl time.Duration) ([]string, error) {
	keep := map[string]bool{}
	if s.Store != nil {
		live, err := s.Store.SuspendedWorkDirs(ctx, time.Now().Add(-ttl))
		if err != nil {
			return nil, err
		}
		for _, d := range live {
			keep[filepath.Clean(d)] = true
		}
	}
	runs, err := os.ReadDir(s.WorkRoot)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []string
	for _, run := range runs {
		runDir := filepath.Join(s.WorkRoot, run.Name())
		if !run.IsDir() {
			if err := os.Remove(runDir); err != nil {
				errs = append(errs, err.Error())
			} else {
				removed = append(removed, runDir)
			}
			continue
		}
		steps, err := os.ReadDir(runDir)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, step := range steps {
			stepDir := filepath.Join(runDir, step.Name())
			if keep[stepDir] {
				continue
			}
			if err := gitenv.RemoveAllWritable(stepDir); err != nil {
				errs = append(errs, err.Error())
			} else {
				removed = append(removed, stepDir)
			}
		}
		if err := os.Remove(runDir); err == nil && len(steps) == 0 {
			removed = append(removed, runDir)
		}
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("sweep %s: %s", s.WorkRoot, strings.Join(errs, "; "))
	}
	return removed, nil
}
