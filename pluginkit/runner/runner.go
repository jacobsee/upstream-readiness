// Package runner executes the commands a git/Go plugin runs inside its
// per-run scratch dir, accumulating a combined log and emitting progress
// around every command. The emit callback keeps the progress payload
// shape plugin-owned.
//
// Every command the Runner executes - logged commands, read-only
// queries, and free-form notes - is also handed to an optional
// Recorder, which is how the plugins keep the complete command history
// of a step (pluginkit/cmdlog) rather than only the bounded tail that
// rides along on progress events.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// EmitFunc publishes one progress update. phase is a short machine-ish
// label, message the current command / activity, log the bounded log
// tail. Implementations must not fail the run (drop on error).
type EmitFunc func(phase, message, log string)

// Kind classifies a recorded entry.
type Kind string

const (
	// KindCommand is a command whose output goes into the run's log
	// (Run, ResolvePseudo).
	KindCommand Kind = "command"
	// KindQuery is a read-only bookkeeping call (Output, ExitCode, …)
	// that is not part of the log tail but is still recorded.
	KindQuery Kind = "query"
	// KindNote is a free-form line written with Note.
	KindNote Kind = "note"
)

// Recorder receives every entry the Runner produces. Start is called
// before the command runs; the returned func exactly once after it ends
// with its exit code (-1 when it did not exit normally: not found,
// killed, cancelled), its complete output, and the error, if any.
// Implementations must not fail the run.
type Recorder interface {
	Start(kind Kind, cwd, cmdLine string) (done func(exitCode int, output []byte, err error))
}

// Runner runs commands with a fixed environment and keeps the combined
// output of every logged command.
type Runner struct {
	Ctx  context.Context
	Env  []string
	Emit EmitFunc
	// Rec, if set, records every command, query, and note.
	Rec Recorder
	log strings.Builder
}

// New returns a Runner. emit may be nil.
func New(ctx context.Context, env []string, emit EmitFunc) *Runner {
	if emit == nil {
		emit = func(string, string, string) {}
	}
	return &Runner{Ctx: ctx, Env: env, Emit: emit}
}

// TailMax bounds the log tail sent with each progress event.
const TailMax = 4096

// Tail returns the last ~4KB of the accumulated command log so
// progress events stay small. The plugins' step-runtime MFEs replace
// their rendered view every event; a bounded tail keeps wire traffic
// sane for hour-long runs. The complete history is the Recorder's job.
func (r *Runner) Tail() string {
	s := r.log.String()
	if len(s) <= TailMax {
		return s
	}
	return "…" + s[len(s)-TailMax:]
}

// Log returns the full accumulated log.
func (r *Runner) Log() string { return r.log.String() }

// Note appends a free-form line (conventionally starting with "# ") to
// the log without running anything.
func (r *Runner) Note(s string) {
	r.log.WriteString(s)
	r.Record(KindNote, "", strings.TrimSpace(s))(0, nil, nil)
}

// Record starts a recorder entry for work the Runner doesn't execute
// itself (e.g. a pipeline). Always returns a callable func.
func (r *Runner) Record(kind Kind, cwd, cmdLine string) func(exitCode int, output []byte, err error) {
	if r.Rec == nil {
		return func(int, []byte, error) {}
	}
	return r.Rec.Start(kind, cwd, cmdLine)
}

// ExitCodeOf maps a command error to the exit code a Recorder gets.
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func cmdLineOf(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

// Run emits BEFORE the command starts (so the UI reflects the
// currently-executing command, not the last-completed one) and again
// AFTER it finishes (so the log tail catches up with its output).
// Without the pre-emit, long-running commands like `go mod tidy` or
// `go mod vendor` leave the UI stuck on the previous command for their
// entire duration.
func (r *Runner) Run(cwd, name string, args ...string) error {
	cmdLine := cmdLineOf(name, args)
	r.Emit("running", cmdLine, r.Tail())
	done := r.Record(KindCommand, cwd, cmdLine)
	cmd := exec.CommandContext(r.Ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = r.Env
	out, runErr := cmd.CombinedOutput()
	done(ExitCodeOf(runErr), out, runErr)
	r.log.WriteString(fmt.Sprintf("$ %s\n%s\n", cmdLine, string(out)))
	r.Emit("running", cmdLine, r.Tail())
	if runErr != nil {
		// Surface the command's captured output alongside the exit
		// status so the workflow's ActivityError carries enough
		// context to debug the failure.
		return fmt.Errorf("%w\n%s", runErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// Output runs a read-only query command (git rev-parse, git log, …)
// and returns its stdout. Not in the log tail: these are bookkeeping
// calls whose output would drown it. Recorded as KindQuery with stdout
// followed by stderr. stderr is included in the error.
func (r *Runner) Output(cwd, name string, args ...string) (string, error) {
	done := r.Record(KindQuery, cwd, cmdLineOf(name, args))
	cmd := exec.CommandContext(r.Ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = r.Env
	stdout, stderr := new(strings.Builder), new(strings.Builder)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	done(ExitCodeOf(err), []byte(stdout.String()+stderr.String()), err)
	if err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ExitCode runs a command silently and returns its exit code. A
// non-exit error (binary missing, ctx cancelled) is returned as err.
// Its output is captured only for the Recorder.
func (r *Runner) ExitCode(cwd, name string, args ...string) (int, error) {
	done := r.Record(KindQuery, cwd, cmdLineOf(name, args))
	cmd := exec.CommandContext(r.Ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = r.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	done(ExitCodeOf(err), out.Bytes(), err)
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// HasStagedChanges reports whether the index differs from HEAD. Uses
// `git diff --cached --quiet`, which exits 0 when clean and 1 when
// there are staged differences.
func (r *Runner) HasStagedChanges(dir string) (bool, error) {
	code, err := r.ExitCode(dir, "git", "diff", "--cached", "--quiet")
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}
	return false, fmt.Errorf("git diff --cached --quiet exited %d", code)
}

// ResolvePseudo resolves a commit SHA on modulePath to its canonical
// module version via `go mod download -json`. `go mod edit -replace`
// refuses raw commit SHAs - only semver tags or pseudo-versions are
// allowed on the RHS.
func (r *Runner) ResolvePseudo(cwd, modulePath, sha string) (string, error) {
	args := []string{"mod", "download", "-json", modulePath + "@" + sha}
	cmdLine := "go " + strings.Join(args, " ")
	r.Emit("running", cmdLine, r.Tail())
	done := r.Record(KindCommand, cwd, cmdLine)
	cmd := exec.CommandContext(r.Ctx, "go", args...)
	cmd.Dir = cwd
	cmd.Env = r.Env
	stdout, stderr := new(strings.Builder), new(strings.Builder)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	done(ExitCodeOf(runErr), []byte(stdout.String()+stderr.String()), runErr)
	r.log.WriteString(fmt.Sprintf("$ %s\n%s%s\n", cmdLine, stdout.String(), stderr.String()))
	r.Emit("running", cmdLine, r.Tail())
	var info struct {
		Version string
		Error   string
	}
	_ = json.Unmarshal([]byte(stdout.String()), &info)
	if runErr != nil {
		if info.Error != "" {
			return "", fmt.Errorf("%w: %s", runErr, info.Error)
		}
		return "", fmt.Errorf("%w\n%s", runErr, strings.TrimSpace(stderr.String()))
	}
	if info.Version == "" {
		return "", fmt.Errorf("go mod download returned no version for %s@%s", modulePath, sha)
	}
	return info.Version, nil
}
