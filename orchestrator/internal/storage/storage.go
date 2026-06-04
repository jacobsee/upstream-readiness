// Package storage is the orchestrator's persistence layer. It owns the
// three things the orchestrator is responsible for: registered plugin
// instances, workflow definitions, and a denormalised summary of runs
// (the authoritative run history lives in Temporal).
//
// Every blob the orchestrator stores on behalf of a plugin - instance
// configuration, step params - is held as a json.RawMessage and never
// inspected. Plugins own the shape of these blobs end-to-end.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the orchestrator's handle to Postgres. All access goes
// through this type so the SQL stays in one file.
type Store struct {
	pool *pgxpool.Pool
}

// Open dials Postgres and runs migrations. dsn is a libpq-style
// connection string, e.g. postgres://user:pass@host:5432/db.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

// ── migrations ────────────────────────────────────────────────────────────────

const currentSchemaVersion = 1

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		return err
	}
	var have int
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&have); err != nil {
		return err
	}
	if have >= currentSchemaVersion {
		return nil
	}
	if have < 1 {
		if _, err := s.pool.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS plugin_instances (
				id              TEXT PRIMARY KEY,
				plugin_id       TEXT NOT NULL,
				display_name    TEXT NOT NULL,
				config_blob     JSONB NOT NULL,
				created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
				updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
			);
			CREATE INDEX IF NOT EXISTS plugin_instances_plugin_id_idx ON plugin_instances (plugin_id);

			CREATE TABLE IF NOT EXISTS workflow_defs (
				id              TEXT PRIMARY KEY,
				display_name    TEXT NOT NULL,
				schedule        TEXT NOT NULL DEFAULT '',
				definition      JSONB NOT NULL,
				created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
				updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
			  removed_at      TIMESTAMPTZ
			);

			CREATE TABLE IF NOT EXISTS run_summaries (
				run_id          TEXT PRIMARY KEY,
				workflow_id     TEXT NOT NULL,
				status          TEXT NOT NULL,
				started_at      TIMESTAMPTZ NOT NULL,
				completed_at    TIMESTAMPTZ,
				results         JSONB
			);
			CREATE INDEX IF NOT EXISTS run_summaries_workflow_id_idx ON run_summaries (workflow_id, started_at DESC);
			CREATE INDEX IF NOT EXISTS run_summaries_started_at_idx ON run_summaries (started_at DESC);

			CREATE TABLE IF NOT EXISTS run_steps (
				run_id          TEXT NOT NULL,
				step_id         TEXT NOT NULL,
				plugin          TEXT NOT NULL,
				action          TEXT NOT NULL,
				status          TEXT NOT NULL,
				started_at      TIMESTAMPTZ NOT NULL,
				completed_at    TIMESTAMPTZ,
				error           TEXT NOT NULL DEFAULT '',
				outputs         JSONB,
			  progress_blob   JSONB,
			  prompt_blob     JSONB,
				PRIMARY KEY (run_id, step_id)
			);
			CREATE INDEX IF NOT EXISTS run_steps_run_id_idx ON run_steps (run_id, started_at);
			CREATE INDEX IF NOT EXISTS run_summaries_started_at_idx ON run_summaries (started_at DESC);
		`); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO schema_version(version) VALUES (1)`); err != nil {
			return err
		}
	}
	return nil
}

// ── plugin instances ──────────────────────────────────────────────────────────

// Instance is an orchestrator-stored handle to a plugin configuration.
// ConfigBlob is owned end-to-end by the plugin; the orchestrator only
// persists it and hands it back on Execute.
type Instance struct {
	ID          string          `json:"id"`
	PluginID    string          `json:"plugin_id"`
	DisplayName string          `json:"display_name"`
	ConfigBlob  json.RawMessage `json:"config_blob"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func (s *Store) UpsertInstance(ctx context.Context, inst Instance) error {
	if inst.ID == "" || inst.PluginID == "" {
		return errors.New("instance id and plugin_id are required")
	}
	if len(inst.ConfigBlob) == 0 {
		inst.ConfigBlob = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO plugin_instances (id, plugin_id, display_name, config_blob)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			config_blob  = EXCLUDED.config_blob,
			updated_at   = now()
	`, inst.ID, inst.PluginID, inst.DisplayName, []byte(inst.ConfigBlob))
	return err
}

func (s *Store) GetInstance(ctx context.Context, id string) (*Instance, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, plugin_id, display_name, config_blob, created_at, updated_at
		  FROM plugin_instances WHERE id = $1
	`, id)
	var inst Instance
	var blob []byte
	err := row.Scan(&inst.ID, &inst.PluginID, &inst.DisplayName, &blob, &inst.CreatedAt, &inst.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inst.ConfigBlob = json.RawMessage(blob)
	return &inst, nil
}

func (s *Store) ListInstances(ctx context.Context, pluginID string) ([]Instance, error) {
	q := `SELECT id, plugin_id, display_name, config_blob, created_at, updated_at FROM plugin_instances`
	args := []any{}
	if pluginID != "" {
		q += ` WHERE plugin_id = $1`
		args = append(args, pluginID)
	}
	q += ` ORDER BY plugin_id, display_name`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		var inst Instance
		var blob []byte
		if err := rows.Scan(&inst.ID, &inst.PluginID, &inst.DisplayName, &blob, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		inst.ConfigBlob = json.RawMessage(blob)
		out = append(out, inst)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM plugin_instances WHERE id = $1`, id)
	return err
}

// ── workflow definitions ──────────────────────────────────────────────────────

// WorkflowRecord stores a workflow definition as opaque JSON. The
// orchestrator decodes it into workflow.Definition only when it needs
// to execute or render it.
type WorkflowRecord struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Schedule    string          `json:"schedule"`
	Definition  json.RawMessage `json:"definition"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	// RemovedAt is set when the workflow's YAML file was no longer in
	// --workflows at startup. A removed workflow can't be run; its history
	// stays until it is purged or the file comes back.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}

// UpsertWorkflow stores a definition loaded from a workflow file. It
// clears removed_at, so a file that comes back restores its workflow
// along with the history it kept.
func (s *Store) UpsertWorkflow(ctx context.Context, wf WorkflowRecord) error {
	if wf.ID == "" {
		return errors.New("workflow id is required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO workflow_defs (id, display_name, schedule, definition)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			schedule     = EXCLUDED.schedule,
			definition   = EXCLUDED.definition,
			removed_at   = NULL,
			updated_at   = now()
	`, wf.ID, wf.DisplayName, wf.Schedule, []byte(wf.Definition))
	return err
}

// MarkWorkflowsRemovedExcept flags every workflow not in keep (and not
// already flagged) as removed. Returns the IDs it flagged.
func (s *Store) MarkWorkflowsRemovedExcept(ctx context.Context, keep []string) ([]string, error) {
	if keep == nil {
		keep = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE workflow_defs SET removed_at = now()
		 WHERE removed_at IS NULL AND NOT (id = ANY($1))
		RETURNING id
	`, keep)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

const workflowCols = `id, display_name, schedule, definition, created_at, updated_at, removed_at`

func scanWorkflow(row pgx.Row) (*WorkflowRecord, error) {
	var wf WorkflowRecord
	var def []byte
	if err := row.Scan(&wf.ID, &wf.DisplayName, &wf.Schedule, &def, &wf.CreatedAt, &wf.UpdatedAt, &wf.RemovedAt); err != nil {
		return nil, err
	}
	wf.Definition = json.RawMessage(def)
	return &wf, nil
}

func (s *Store) GetWorkflow(ctx context.Context, id string) (*WorkflowRecord, error) {
	wf, err := scanWorkflow(s.pool.QueryRow(ctx, `SELECT `+workflowCols+` FROM workflow_defs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return wf, err
}

func (s *Store) ListWorkflows(ctx context.Context) ([]WorkflowRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+workflowCols+` FROM workflow_defs ORDER BY display_name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowRecord
	for rows.Next() {
		wf, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *wf)
	}
	return out, rows.Err()
}

// WorkflowRunRef is the little a purge needs to know about each run.
type WorkflowRunRef struct {
	RunID  string
	Status string
}

// ListAllRunsForWorkflow returns every run of a workflow, unpaged.
func (s *Store) ListAllRunsForWorkflow(ctx context.Context, workflowID string) ([]WorkflowRunRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT run_id, status FROM run_summaries WHERE workflow_id = $1`, workflowID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkflowRunRef, error) {
		var r WorkflowRunRef
		err := row.Scan(&r.RunID, &r.Status)
		return r, err
	})
}

// PluginsUsedByWorkflow returns the distinct plugin IDs any recorded
// step of the workflow's runs ran on.
func (s *Store) PluginsUsedByWorkflow(ctx context.Context, workflowID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT rs.plugin
		  FROM run_steps rs JOIN run_summaries r ON r.run_id = rs.run_id
		 WHERE r.workflow_id = $1 AND rs.plugin <> ''
	`, workflowID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ErrWorkflowNotRemoved is returned by PurgeWorkflow for a workflow that
// is still active (or no longer exists).
var ErrWorkflowNotRemoved = errors.New("workflow is not removed")

// PurgeWorkflow deletes a removed workflow's definition, run summaries,
// and run steps in one transaction. Returns rows deleted per table.
func (s *Store) PurgeWorkflow(ctx context.Context, workflowID string) (map[string]int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock the row so a concurrent restart can't restore the workflow
	// halfway through.
	var removed *time.Time
	err = tx.QueryRow(ctx, `SELECT removed_at FROM workflow_defs WHERE id = $1 FOR UPDATE`, workflowID).Scan(&removed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && removed == nil) {
		return nil, ErrWorkflowNotRemoved
	}
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, q := range []struct{ key, sql string }{
		{"run_steps", `DELETE FROM run_steps WHERE run_id IN (SELECT run_id FROM run_summaries WHERE workflow_id = $1)`},
		{"run_summaries", `DELETE FROM run_summaries WHERE workflow_id = $1`},
		{"workflow_defs", `DELETE FROM workflow_defs WHERE id = $1`},
	} {
		tag, err := tx.Exec(ctx, q.sql, workflowID)
		if err != nil {
			return nil, fmt.Errorf("delete %s: %w", q.key, err)
		}
		counts[q.key] = tag.RowsAffected()
	}
	return counts, tx.Commit(ctx)
}

// ── run summaries ─────────────────────────────────────────────────────────────

// RunSummary mirrors what the Temporal workflow records when it finishes
// (or fails). Temporal is authoritative for execution state; this table
// exists so the orchestrator UI can list runs without paging Temporal
// history for every request.
type RunSummary struct {
	RunID       string          `json:"run_id"`
	WorkflowID  string          `json:"workflow_id"`
	Status      string          `json:"status"` // running | succeeded | failed
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Results     json.RawMessage `json:"results,omitempty"`
	// AwaitingSteps counts this run's steps currently parked in
	// awaiting_resolution. Populated by the list queries only, so the
	// runs dashboards can flag runs that need human input.
	AwaitingSteps int `json:"awaiting_steps,omitempty"`
}

// awaitingStepsExpr is the correlated subquery the run-list queries use
// to populate RunSummary.AwaitingSteps.
const awaitingStepsExpr = `(SELECT COUNT(*) FROM run_steps rs WHERE rs.run_id = run_summaries.run_id AND rs.status = 'awaiting_resolution')`

func (s *Store) StartRun(ctx context.Context, r RunSummary) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_summaries (run_id, workflow_id, status, started_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id) DO NOTHING
	`, r.RunID, r.WorkflowID, r.Status, r.StartedAt)
	return err
}

func (s *Store) FinishRun(ctx context.Context, runID, status string, completedAt time.Time, results json.RawMessage) error {
	if len(results) == 0 {
		results = json.RawMessage(`null`)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE run_summaries
		   SET status = $2, completed_at = $3, results = $4
		 WHERE run_id = $1
	`, runID, status, completedAt, []byte(results))
	return err
}

func (s *Store) GetRun(ctx context.Context, runID string) (*RunSummary, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT run_id, workflow_id, status, started_at, completed_at, results
		  FROM run_summaries WHERE run_id = $1
	`, runID)
	var r RunSummary
	var results []byte
	var completedAt *time.Time
	err := row.Scan(&r.RunID, &r.WorkflowID, &r.Status, &r.StartedAt, &completedAt, &results)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CompletedAt = completedAt
	if len(results) > 0 {
		r.Results = json.RawMessage(results)
	}
	return &r, nil
}

// ListRuns returns recent runs across all workflows, newest first. If
// status is non-empty, results are filtered to that status. The dashboard
// uses this for its cross-workflow "Runs" view.
func (s *Store) ListRuns(ctx context.Context, status string, limit int) ([]RunSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT run_id, workflow_id, status, started_at, completed_at, results, ` + awaitingStepsExpr + ` FROM run_summaries`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY started_at DESC LIMIT $` + fmt.Sprintf("%d", len(args)+1)
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		var results []byte
		var completedAt *time.Time
		if err := rows.Scan(&r.RunID, &r.WorkflowID, &r.Status, &r.StartedAt, &completedAt, &results, &r.AwaitingSteps); err != nil {
			return nil, err
		}
		r.CompletedAt = completedAt
		if len(results) > 0 {
			r.Results = json.RawMessage(results)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ListRunsForWorkflow(ctx context.Context, workflowID string, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, workflow_id, status, started_at, completed_at, results, `+awaitingStepsExpr+`
		  FROM run_summaries
		 WHERE workflow_id = $1
		 ORDER BY started_at DESC
		 LIMIT $2
	`, workflowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		var results []byte
		var completedAt *time.Time
		if err := rows.Scan(&r.RunID, &r.WorkflowID, &r.Status, &r.StartedAt, &completedAt, &results, &r.AwaitingSteps); err != nil {
			return nil, err
		}
		r.CompletedAt = completedAt
		if len(results) > 0 {
			r.Results = json.RawMessage(results)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── run steps ─────────────────────────────────────────────────────────────────

// StepRecord is one row in the run_steps table - a per-step lifecycle
// event tied to a specific run. Written when a step transitions from
// pending → running (UpsertStepStart) and again when it reaches a
// terminal state (UpsertStepFinish). Progress is the most recent
// plugin-emitted progress payload (opaque bytes).
type StepRecord struct {
	RunID       string          `json:"run_id"`
	StepID      string          `json:"step_id"`
	Plugin      string          `json:"plugin"`
	Action      string          `json:"action"`
	Status      string          `json:"status"` // running | awaiting_resolution | succeeded | failed | skipped | cancelled
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Error       string          `json:"error,omitempty"`
	Outputs     json.RawMessage `json:"outputs,omitempty"`
	Progress    json.RawMessage `json:"progress,omitempty"`
	// Prompt is the opaque ExecuteSuspend payload while the step is
	// awaiting_resolution; nil otherwise.
	Prompt json.RawMessage `json:"prompt,omitempty"`
}

// UpsertStepStart records that a step has started. If a row already
// exists for (run_id, step_id) - e.g. because Temporal retried the
// activity - the started_at and plugin/action are refreshed but the
// terminal state (if any) is not clobbered. A step resuming from
// awaiting_resolution keeps its original started_at and has its prompt
// cleared.
func (s *Store) UpsertStepStart(ctx context.Context, r StepRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_steps (run_id, step_id, plugin, action, status, started_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (run_id, step_id) DO UPDATE SET
			plugin     = EXCLUDED.plugin,
			action     = EXCLUDED.action,
			status      = CASE WHEN run_steps.completed_at IS NULL THEN EXCLUDED.status ELSE run_steps.status END,
			started_at  = CASE WHEN run_steps.status = 'awaiting_resolution' THEN run_steps.started_at ELSE EXCLUDED.started_at END,
			prompt_blob = NULL
	`, r.RunID, r.StepID, r.Plugin, r.Action, r.Status, r.StartedAt)
	return err
}

// UpsertStepFinish records that a step has reached a terminal state.
// Called from RecordStepFinish activity after ExecuteActivity settles
// (including for skipped steps whose deps failed).
func (s *Store) UpsertStepFinish(ctx context.Context, runID, stepID, status, stepErr string, outputs json.RawMessage, completedAt time.Time) error {
	if len(outputs) == 0 {
		outputs = json.RawMessage(`null`)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_steps (run_id, step_id, plugin, action, status, started_at, completed_at, error, outputs)
		VALUES ($1, $2, '', '', $3, $6, $6, $4, $5)
		ON CONFLICT (run_id, step_id) DO UPDATE SET
			status       = EXCLUDED.status,
			completed_at = EXCLUDED.completed_at,
			error        = EXCLUDED.error,
			outputs      = EXCLUDED.outputs,
			prompt_blob  = NULL
	`, runID, stepID, status, stepErr, []byte(outputs), completedAt)
	return err
}

// ListStepsForRun returns every recorded step for a run in start order.
// GetRunStatus uses this to overlay live step state on the run summary
// while the workflow is still running.
func (s *Store) ListStepsForRun(ctx context.Context, runID string) ([]StepRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, step_id, plugin, action, status, started_at, completed_at, error, outputs, progress_blob, prompt_blob
		  FROM run_steps
		 WHERE run_id = $1
		 ORDER BY started_at ASC, step_id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StepRecord
	for rows.Next() {
		var r StepRecord
		var completedAt *time.Time
		var outputs, progress, prompt []byte
		if err := rows.Scan(&r.RunID, &r.StepID, &r.Plugin, &r.Action, &r.Status, &r.StartedAt, &completedAt, &r.Error, &outputs, &progress, &prompt); err != nil {
			return nil, err
		}
		r.CompletedAt = completedAt
		if len(outputs) > 0 && string(outputs) != "null" {
			r.Outputs = json.RawMessage(outputs)
		}
		if len(progress) > 0 && string(progress) != "null" {
			r.Progress = json.RawMessage(progress)
		}
		if len(prompt) > 0 && string(prompt) != "null" {
			r.Prompt = json.RawMessage(prompt)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertStepProgress overwrites the progress_blob for a running step.
// Called from RunStep on every progress event so a page reload or a
// late SSE subscriber can bootstrap the latest state.
func (s *Store) UpsertStepProgress(ctx context.Context, runID, stepID string, payload json.RawMessage) error {
	if len(payload) == 0 {
		payload = json.RawMessage(`null`)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_steps (run_id, step_id, plugin, action, status, started_at, progress_blob)
		VALUES ($1, $2, '', '', 'running', now(), $3)
		ON CONFLICT (run_id, step_id) DO UPDATE SET
			progress_blob = EXCLUDED.progress_blob
	`, runID, stepID, []byte(payload))
	return err
}

// UpsertStepAwaiting parks a step in awaiting_resolution with the opaque
// prompt the plugin emitted via ExecuteSuspend. Written by the
// RecordStepAwaiting activity; cleared by the next UpsertStepStart (on
// resume) or UpsertStepFinish (on cancellation).
func (s *Store) UpsertStepAwaiting(ctx context.Context, runID, stepID string, prompt json.RawMessage) error {
	if len(prompt) == 0 {
		prompt = json.RawMessage(`null`)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_steps (run_id, step_id, plugin, action, status, started_at, prompt_blob)
		VALUES ($1, $2, '', '', 'awaiting_resolution', now(), $3)
		ON CONFLICT (run_id, step_id) DO UPDATE SET
			status      = 'awaiting_resolution',
			prompt_blob = EXCLUDED.prompt_blob
	`, runID, stepID, []byte(prompt))
	return err
}

// GetStepStatus returns the status of one run step, or "" when no row
// exists. Used to gate the resolve endpoint.
func (s *Store) GetStepStatus(ctx context.Context, runID, stepID string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM run_steps WHERE run_id = $1 AND step_id = $2`, runID, stepID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}
