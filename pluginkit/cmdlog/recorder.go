package cmdlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"time"

	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// writeTimeout bounds each history write. Writes use a context detached
// from the Execute call's, so a cancelled run still records how its
// last command ended.
const writeTimeout = 10 * time.Second

// Recorder is a runner.Recorder for one Execute call: every entry is
// written under the request's (workflow_run_id, step_id) with a fresh
// execution_id. Share one Recorder across every Runner the call
// creates so seq stays in order.
//
// Commands are inserted when they start and completed when they end, so
// one that hangs or is killed still appears (unfinished). Queries and
// notes are short and numerous, so they are written once, when done.
//
// Write failures never fail the run; the first one per execution is
// logged.
type Recorder struct {
	s           *Store
	ctx         context.Context
	base        Entry
	mu          sync.Mutex
	seq         int
	warnedWrite bool
}

// NewRecorder returns a Recorder for req, or nil (which callers can
// assign to runner.Runner.Rec as-is: a nil *Recorder records nothing)
// when s is nil.
func NewRecorder(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest) *Recorder {
	if s == nil {
		return nil
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Recorder{
		s:   s,
		ctx: context.WithoutCancel(ctx),
		base: Entry{
			WorkflowRunID: req.GetWorkflowRunId(),
			StepID:        req.GetStepId(),
			WorkflowID:    req.GetWorkflowId(),
			ExecutionID:   "x_" + hex.EncodeToString(b),
			Attempt:       int(req.GetAttempt()),
		},
	}
}

func (r *Recorder) warn(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.warnedWrite {
		r.warnedWrite = true
		log.Printf("cmdlog: recording history for %s/%s (%s) failed, later failures not logged: %v",
			r.base.WorkflowRunID, r.base.StepID, r.base.ExecutionID, err)
	}
}

func (r *Recorder) Start(kind runner.Kind, cwd, cmdLine string) func(exitCode int, output []byte, err error) {
	if r == nil {
		return func(int, []byte, error) {}
	}
	r.mu.Lock()
	r.seq++
	e := r.base
	e.Seq, e.Kind, e.Cwd, e.Command, e.StartedAt = r.seq, string(kind), cwd, cmdLine, time.Now().UTC()
	r.mu.Unlock()

	var id int64
	if kind == runner.KindCommand {
		ctx, cancel := context.WithTimeout(r.ctx, writeTimeout)
		var err error
		id, err = r.s.insert(ctx, e, nil)
		cancel()
		if err != nil {
			r.warn(err)
		}
	}
	var once sync.Once
	return func(exitCode int, output []byte, cmdErr error) {
		once.Do(func() {
			now := time.Now().UTC()
			msg := ""
			if cmdErr != nil {
				msg = cmdErr.Error()
			}
			ctx, cancel := context.WithTimeout(r.ctx, writeTimeout)
			defer cancel()
			var err error
			if id != 0 {
				err = r.s.finish(ctx, id, now, exitCode, msg, output)
			} else {
				e.FinishedAt, e.ExitCode, e.Error = &now, &exitCode, msg
				_, err = r.s.insert(ctx, e, output)
			}
			if err != nil {
				r.warn(err)
			}
		})
	}
}

// Attach makes r record everything rn runs. Safe with a nil r.
func (r *Recorder) Attach(rn *runner.Runner) {
	if r != nil {
		rn.Rec = r
	}
}
