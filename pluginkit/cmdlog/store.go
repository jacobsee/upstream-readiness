// Package cmdlog is the complete, permanent command history of every
// step a git/Go plugin runs: each command, read-only query, and note
// the step's runner.Runner produced, with its working dir, timing, exit
// code, and full output. Progress events only carry a bounded log tail
// (runner.TailMax); this is where the rest lives, so a run's every
// command and result stays viewable after the fact.
//
// Same shape as pluginkit/patchqueue: storage against the plugin's own
// Postgres (the plugin hands in its pool and runs TableSQL in its own
// versioned migrations), a runner.Recorder that writes to it, and the
// cmdlog_* MFE-backing action handlers. Rows are keyed by
// (workflow_run_id, step_id) - the scope the shell's step-runtime slot
// already has for past runs - and grouped by execution: every Execute
// call (first run, each resume after a suspend, each Temporal retry) is
// a new execution_id. Rows are never deleted by the plugin; ReleaseStep
// drops working copies, not history.
package cmdlog

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TableSQL creates the command_log table. Idempotent. Plugins run it
// inside their own schema_version-gated migration block.
//
// output is BYTEA, not TEXT: command output may hold NUL bytes or
// invalid UTF-8 (binary diffs, git show of a blob), which Postgres TEXT
// rejects. It is converted to valid UTF-8 only when served.
const TableSQL = `
	CREATE TABLE IF NOT EXISTS command_log (
		id              BIGSERIAL PRIMARY KEY,
		workflow_run_id TEXT NOT NULL,
		step_id         TEXT NOT NULL,
		workflow_id     TEXT NOT NULL DEFAULT '',
		execution_id    TEXT NOT NULL,
		attempt         INT  NOT NULL DEFAULT 0,
		seq             INT  NOT NULL,
		kind            TEXT NOT NULL,
		cwd             TEXT NOT NULL DEFAULT '',
		command         TEXT NOT NULL,
		started_at      TIMESTAMPTZ NOT NULL,
		finished_at     TIMESTAMPTZ,
		exit_code       INT,
		error           TEXT NOT NULL DEFAULT '',
		output          BYTEA NOT NULL DEFAULT ''::bytea,
		output_bytes    BIGINT NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS command_log_step_idx
	    ON command_log (workflow_run_id, step_id, id);
`

// Store reads and writes command_log on the plugin's own pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pool whose database already has TableSQL applied.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Entry is one recorded command / query / note. Output is only loaded
// by Get; List leaves it empty and reports OutputBytes.
type Entry struct {
	ID            int64      `json:"id"`
	WorkflowRunID string     `json:"workflow_run_id"`
	StepID        string     `json:"step_id"`
	WorkflowID    string     `json:"workflow_id"`
	ExecutionID   string     `json:"execution_id"`
	Attempt       int        `json:"attempt"`
	Seq           int        `json:"seq"`
	Kind          string     `json:"kind"`
	Cwd           string     `json:"cwd"`
	Command       string     `json:"command"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Error         string     `json:"error,omitempty"`
	OutputBytes   int64      `json:"output_bytes"`
	Output        string     `json:"output,omitempty"`
}

// insert writes e (finished or not) and returns its id.
func (s *Store) insert(ctx context.Context, e Entry, output []byte) (int64, error) {
	if output == nil {
		output = []byte{}
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO command_log (workflow_run_id, step_id, workflow_id, execution_id, attempt, seq, kind, cwd, command,
			started_at, finished_at, exit_code, error, output, output_bytes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING id`,
		e.WorkflowRunID, e.StepID, e.WorkflowID, e.ExecutionID, e.Attempt, e.Seq, e.Kind, e.Cwd, e.Command,
		e.StartedAt, e.FinishedAt, e.ExitCode, e.Error, output, int64(len(output)),
	).Scan(&id)
	return id, err
}

// finish completes a row inserted unfinished.
func (s *Store) finish(ctx context.Context, id int64, finishedAt time.Time, exitCode int, errMsg string, output []byte) error {
	if output == nil {
		output = []byte{}
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE command_log SET finished_at=$2, exit_code=$3, error=$4, output=$5, output_bytes=$6 WHERE id=$1`,
		id, finishedAt, exitCode, errMsg, output, int64(len(output)))
	return err
}

// ListOptions narrows List.
type ListOptions struct {
	// AfterID returns only rows with id > AfterID (pagination / live
	// catch-up).
	AfterID int64
	// Limit caps the page; defaults to 500, max 2000.
	Limit int
	// Kinds restricts to these kinds; empty means all.
	Kinds []string
	// Query matches the command line (case-insensitive) or the output
	// (exact bytes).
	Query string
}

// Page is one List result.
type Page struct {
	Entries []Entry `json:"entries"`
	// More is true when rows past the last entry match too.
	More bool `json:"more"`
	// Counts is the number of rows per kind for the whole (run, step),
	// ignoring the filters.
	Counts map[string]int `json:"counts"`
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 500
	case n > 2000:
		return 2000
	}
	return n
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// List returns a page of a (run, step)'s history in execution order,
// without output bodies.
func (s *Store) List(ctx context.Context, runID, stepID string, o ListOptions) (*Page, error) {
	if runID == "" || stepID == "" {
		return nil, errors.New("workflow_run_id and step_id are required")
	}
	limit := clampLimit(o.Limit)
	kinds := o.Kinds
	if kinds == nil {
		kinds = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workflow_run_id, step_id, workflow_id, execution_id, attempt, seq, kind, cwd, command,
		       started_at, finished_at, exit_code, error, output_bytes
		  FROM command_log
		 WHERE workflow_run_id=$1 AND step_id=$2 AND id > $3
		   AND (cardinality($4::text[]) = 0 OR kind = ANY($4))
		   AND ($5 = '' OR command ILIKE '%' || $6 || '%' OR position(convert_to($5, 'UTF8') IN output) > 0)
		 ORDER BY id
		 LIMIT $7`,
		runID, stepID, o.AfterID, kinds, o.Query, likeEscaper.Replace(o.Query), limit+1)
	if err != nil {
		return nil, err
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.ID, &e.WorkflowRunID, &e.StepID, &e.WorkflowID, &e.ExecutionID, &e.Attempt, &e.Seq, &e.Kind,
			&e.Cwd, &e.Command, &e.StartedAt, &e.FinishedAt, &e.ExitCode, &e.Error, &e.OutputBytes)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	p := &Page{Entries: entries, Counts: map[string]int{}}
	if len(p.Entries) > limit {
		p.Entries, p.More = p.Entries[:limit], true
	}
	crows, err := s.pool.Query(ctx, `
		SELECT kind, count(*) FROM command_log WHERE workflow_run_id=$1 AND step_id=$2 GROUP BY kind`, runID, stepID)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var k string
		var n int
		if err := crows.Scan(&k, &n); err != nil {
			return nil, err
		}
		p.Counts[k] = n
	}
	return p, crows.Err()
}

// DeleteWorkflow deletes the history of every run of a workflow. Only
// for Plugin.PurgeWorkflow: history is otherwise never deleted.
func (s *Store) DeleteWorkflow(ctx context.Context, workflowID string) (int64, error) {
	if workflowID == "" {
		return 0, errors.New("workflow_id is required")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM command_log WHERE workflow_id=$1`, workflowID)
	return tag.RowsAffected(), err
}

// Get returns one entry with its full output, scoped to (run, step) so
// a caller can't read another step's history by guessing ids. nil when
// absent.
func (s *Store) Get(ctx context.Context, runID, stepID string, id int64) (*Entry, error) {
	var e Entry
	var out []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, workflow_run_id, step_id, workflow_id, execution_id, attempt, seq, kind, cwd, command,
		       started_at, finished_at, exit_code, error, output_bytes, output
		  FROM command_log WHERE id=$1 AND workflow_run_id=$2 AND step_id=$3`, id, runID, stepID,
	).Scan(&e.ID, &e.WorkflowRunID, &e.StepID, &e.WorkflowID, &e.ExecutionID, &e.Attempt, &e.Seq, &e.Kind,
		&e.Cwd, &e.Command, &e.StartedAt, &e.FinishedAt, &e.ExitCode, &e.Error, &e.OutputBytes, &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.Output = strings.ToValidUTF8(string(out), "�")
	return &e, nil
}
