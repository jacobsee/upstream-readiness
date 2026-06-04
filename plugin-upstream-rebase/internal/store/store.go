// Package store is the upstream-rebase plugin's own persistence layer
// (plugin-owned Postgres; the orchestrator has no handle to it).
//
// Tables:
//   - step_patches   - user-defined commits per (workflow_id, step_id),
//     shared schema/queries via pluginkit/patchqueue.
//   - rebase_runs    - one row per (workflow_run_id, step_id): what was
//     rebased onto what, phase, resume checkpoint,
//     range-diff.
//   - rebase_commits - the commit table the UI shows: every commit the
//     run had to consider, in pick order, with status.
//   - conflicts      - captured conflicts awaiting / past resolution.
//   - resolutions    - saved conflict resolutions, keyed by exact
//     conflict fingerprint per (workflow_id, step_id);
//     reused automatically on every later run.
//   - decisions      - human decisions for commits that can't be
//     handled mechanically (unclassified / malformed /
//     evil merge / carry that became empty).
//   - rerere_entries - the git rerere cache per (workflow_id, step_id).
//   - command_log    - every command / query / note each step ran, with
//     full output, per (workflow_run_id, step_id); schema
//     and queries shared via pluginkit/cmdlog.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jacobsee/upstream-readiness/pluginkit/cmdlog"
	"github.com/jacobsee/upstream-readiness/pluginkit/patchqueue"
)

type Store struct {
	pool *pgxpool.Pool
	// Patches is the per-(workflow_id, step_id) patch queue.
	Patches *patchqueue.Store
	// Commands is every step's complete command history.
	Commands *cmdlog.Store
}

// Open dials Postgres and applies migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	s := &Store{pool: pool, Patches: patchqueue.NewStore(pool), Commands: cmdlog.NewStore(pool)}
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
	if _, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
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
		if _, err := s.pool.Exec(ctx, patchqueue.TableSQL); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS rebase_runs (
				workflow_run_id TEXT NOT NULL,
				step_id         TEXT NOT NULL,
				workflow_id     TEXT NOT NULL,
				instance_id     TEXT NOT NULL,
				clone_url       TEXT NOT NULL DEFAULT '',
				upstream_url    TEXT NOT NULL DEFAULT '',
				fork_branch     TEXT NOT NULL DEFAULT '',
				fork_tip        TEXT NOT NULL DEFAULT '',
				target_ref      TEXT NOT NULL DEFAULT '',
				target_sha      TEXT NOT NULL DEFAULT '',
				prev_merge      TEXT NOT NULL DEFAULT '',
				prev_base       TEXT NOT NULL DEFAULT '',
				prev_base_tag   TEXT NOT NULL DEFAULT '',
				branch          TEXT NOT NULL DEFAULT '',
				new_merge       TEXT NOT NULL DEFAULT '',
				phase           TEXT NOT NULL DEFAULT '',
				status          TEXT NOT NULL DEFAULT 'running',
				checkpoint_head TEXT NOT NULL DEFAULT '',
				work_dir        TEXT NOT NULL DEFAULT '',
				pushed_sha      TEXT NOT NULL DEFAULT '',
				range_diff      TEXT NOT NULL DEFAULT '',
				error           TEXT NOT NULL DEFAULT '',
				table_rev       BIGINT NOT NULL DEFAULT 0,
				started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
				updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
				finished_at     TIMESTAMPTZ,
				PRIMARY KEY (workflow_run_id, step_id)
			);
			CREATE TABLE IF NOT EXISTS rebase_commits (
				workflow_run_id   TEXT NOT NULL,
				step_id           TEXT NOT NULL,
				ordinal           INT  NOT NULL,
				orig_sha          TEXT NOT NULL,
				author            TEXT NOT NULL DEFAULT '',
				subject           TEXT NOT NULL DEFAULT '',
				kind              TEXT NOT NULL,
				pr_number         INT  NOT NULL DEFAULT 0,
				patch_id          TEXT NOT NULL DEFAULT '',
				detail            TEXT NOT NULL DEFAULT '',
				status            TEXT NOT NULL DEFAULT 'pending',
				new_sha           TEXT NOT NULL DEFAULT '',
				resolution_id     TEXT NOT NULL DEFAULT '',
				resolution_source TEXT NOT NULL DEFAULT '',
				note              TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (workflow_run_id, step_id, ordinal)
			);
			CREATE TABLE IF NOT EXISTS conflicts (
				conflict_id     TEXT PRIMARY KEY,
				workflow_run_id TEXT NOT NULL,
				step_id         TEXT NOT NULL,
				workflow_id     TEXT NOT NULL,
				ordinal         INT  NOT NULL,
				orig_sha        TEXT NOT NULL,
				subject         TEXT NOT NULL DEFAULT '',
				fingerprint     TEXT NOT NULL,
				files           JSONB NOT NULL,
				status          TEXT NOT NULL DEFAULT 'open',
				created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
				resolved_at     TIMESTAMPTZ
			);
			CREATE INDEX IF NOT EXISTS conflicts_run_idx ON conflicts (workflow_run_id, step_id, status);
			CREATE TABLE IF NOT EXISTS resolutions (
				resolution_id  TEXT PRIMARY KEY,
				workflow_id    TEXT NOT NULL,
				step_id        TEXT NOT NULL,
				fingerprint    TEXT NOT NULL,
				subject        TEXT NOT NULL DEFAULT '',
				orig_sha       TEXT NOT NULL DEFAULT '',
				kind           TEXT NOT NULL,
				files          JSONB NOT NULL DEFAULT '{}',
				reason         TEXT NOT NULL DEFAULT '',
				resolved_by    TEXT NOT NULL DEFAULT '',
				created_in_run TEXT NOT NULL DEFAULT '',
				use_count      INT  NOT NULL DEFAULT 0,
				created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
				updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
				last_used_at   TIMESTAMPTZ,
				UNIQUE (workflow_id, step_id, fingerprint)
			);
			CREATE TABLE IF NOT EXISTS decisions (
				workflow_id TEXT NOT NULL,
				step_id     TEXT NOT NULL,
				orig_sha    TEXT NOT NULL,
				question    TEXT NOT NULL,
				decision    TEXT NOT NULL,
				reason      TEXT NOT NULL DEFAULT '',
				subject     TEXT NOT NULL DEFAULT '',
				decided_by  TEXT NOT NULL DEFAULT '',
				created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
				PRIMARY KEY (workflow_id, step_id, orig_sha, question)
			);
			CREATE TABLE IF NOT EXISTS rerere_entries (
				workflow_id   TEXT NOT NULL,
				step_id       TEXT NOT NULL,
				entry_id      TEXT NOT NULL,
				files         JSONB NOT NULL,
				resolution_id TEXT NOT NULL DEFAULT '',
				subject       TEXT NOT NULL DEFAULT '',
				path          TEXT NOT NULL DEFAULT '',
				updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
				PRIMARY KEY (workflow_id, step_id, entry_id)
			);
			CREATE INDEX IF NOT EXISTS rerere_entries_resolution_idx ON rerere_entries (resolution_id);
		`); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, cmdlog.TableSQL); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO schema_version(version) VALUES (1)`); err != nil {
			return err
		}
	}
	return nil
}

// NewID returns a random id with the given prefix.
func NewID(prefix string) string {
	var buf [10]byte
	_, _ = rand.Read(buf[:])
	return prefix + hex.EncodeToString(buf[:])
}

// ── runs ──────────────────────────────────────────────────────────────────────

// Run is one rebase execution's bookkeeping row.
type Run struct {
	WorkflowRunID  string     `json:"workflow_run_id"`
	StepID         string     `json:"step_id"`
	WorkflowID     string     `json:"workflow_id"`
	InstanceID     string     `json:"instance_id"`
	CloneURL       string     `json:"clone_url"`
	UpstreamURL    string     `json:"upstream_url"`
	ForkBranch     string     `json:"fork_branch"`
	ForkTip        string     `json:"fork_tip"`
	TargetRef      string     `json:"target_ref"`
	TargetSHA      string     `json:"target_sha"`
	PrevMerge      string     `json:"prev_merge"`
	PrevBase       string     `json:"prev_base"`
	PrevBaseTag    string     `json:"prev_base_tag"`
	Branch         string     `json:"branch"`
	NewMerge       string     `json:"new_merge"`
	Phase          string     `json:"phase"`
	Status         string     `json:"status"` // running | suspended | succeeded | failed | cancelled
	CheckpointHead string     `json:"checkpoint_head"`
	WorkDir        string     `json:"-"`
	PushedSHA      string     `json:"pushed_sha"`
	RangeDiff      string     `json:"range_diff,omitempty"`
	Error          string     `json:"error,omitempty"`
	TableRev       int64      `json:"table_rev"`
	StartedAt      time.Time  `json:"started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

const runCols = `workflow_run_id, step_id, workflow_id, instance_id, clone_url, upstream_url, fork_branch, fork_tip,
	target_ref, target_sha, prev_merge, prev_base, prev_base_tag, branch, new_merge, phase, status,
	checkpoint_head, work_dir, pushed_sha, range_diff, error, table_rev, started_at, updated_at, finished_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.WorkflowRunID, &r.StepID, &r.WorkflowID, &r.InstanceID, &r.CloneURL, &r.UpstreamURL, &r.ForkBranch, &r.ForkTip,
		&r.TargetRef, &r.TargetSHA, &r.PrevMerge, &r.PrevBase, &r.PrevBaseTag, &r.Branch, &r.NewMerge, &r.Phase, &r.Status,
		&r.CheckpointHead, &r.WorkDir, &r.PushedSHA, &r.RangeDiff, &r.Error, &r.TableRev, &r.StartedAt, &r.UpdatedAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// GetRun returns the run row, or nil.
func (s *Store) GetRun(ctx context.Context, runID, stepID string) (*Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM rebase_runs WHERE workflow_run_id=$1 AND step_id=$2`, runID, stepID))
}

// SaveRun upserts every column of r (except started_at, which is kept
// from the first insert) and bumps updated_at.
func (s *Store) SaveRun(ctx context.Context, r *Run) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rebase_runs (workflow_run_id, step_id, workflow_id, instance_id, clone_url, upstream_url, fork_branch, fork_tip,
			target_ref, target_sha, prev_merge, prev_base, prev_base_tag, branch, new_merge, phase, status,
			checkpoint_head, work_dir, pushed_sha, range_diff, error, table_rev, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		ON CONFLICT (workflow_run_id, step_id) DO UPDATE SET
			workflow_id=EXCLUDED.workflow_id, instance_id=EXCLUDED.instance_id, clone_url=EXCLUDED.clone_url,
			upstream_url=EXCLUDED.upstream_url, fork_branch=EXCLUDED.fork_branch, fork_tip=EXCLUDED.fork_tip,
			target_ref=EXCLUDED.target_ref, target_sha=EXCLUDED.target_sha, prev_merge=EXCLUDED.prev_merge,
			prev_base=EXCLUDED.prev_base, prev_base_tag=EXCLUDED.prev_base_tag, branch=EXCLUDED.branch,
			new_merge=EXCLUDED.new_merge, phase=EXCLUDED.phase, status=EXCLUDED.status,
			checkpoint_head=EXCLUDED.checkpoint_head, work_dir=EXCLUDED.work_dir, pushed_sha=EXCLUDED.pushed_sha,
			range_diff=EXCLUDED.range_diff, error=EXCLUDED.error, table_rev=EXCLUDED.table_rev,
			finished_at=EXCLUDED.finished_at, updated_at=now()
	`, r.WorkflowRunID, r.StepID, r.WorkflowID, r.InstanceID, r.CloneURL, r.UpstreamURL, r.ForkBranch, r.ForkTip,
		r.TargetRef, r.TargetSHA, r.PrevMerge, r.PrevBase, r.PrevBaseTag, r.Branch, r.NewMerge, r.Phase, r.Status,
		r.CheckpointHead, r.WorkDir, r.PushedSHA, r.RangeDiff, r.Error, r.TableRev, r.FinishedAt)
	return err
}

// CloseOpenRun marks a run still recorded as running / suspended as
// cancelled. Used when the orchestrator says the step is over; a run
// that already finished is left as it is.
func (s *Store) CloseOpenRun(ctx context.Context, runID, stepID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE rebase_runs SET status='cancelled', error=$3, finished_at=now(), updated_at=now()
		WHERE workflow_run_id=$1 AND step_id=$2 AND status IN ('running','suspended')`, runID, stepID, reason)
	return err
}

// SuspendedWorkDirs lists work dirs of runs still suspended and touched
// since since - the only working copies worth keeping.
func (s *Store) SuspendedWorkDirs(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT work_dir FROM rebase_runs WHERE work_dir <> '' AND status='suspended' AND updated_at >= $1`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ── commit table ──────────────────────────────────────────────────────────────

// CommitRow is one row of the commit table.
type CommitRow struct {
	Ordinal          int    `json:"ordinal"`
	OrigSHA          string `json:"orig_sha"`
	Author           string `json:"author"`
	Subject          string `json:"subject"`
	Kind             string `json:"kind"`
	PR               int    `json:"pr,omitempty"`
	PatchID          string `json:"patch_id,omitempty"`
	Detail           string `json:"detail,omitempty"`
	Status           string `json:"status"`
	NewSHA           string `json:"new_sha,omitempty"`
	ResolutionID     string `json:"resolution_id,omitempty"`
	ResolutionSource string `json:"resolution_source,omitempty"`
	Note             string `json:"note,omitempty"`
}

// ReplaceCommits atomically replaces a run's commit table.
func (s *Store) ReplaceCommits(ctx context.Context, runID, stepID string, rows []CommitRow) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM rebase_commits WHERE workflow_run_id=$1 AND step_id=$2`, runID, stepID); err != nil {
		return err
	}
	for _, c := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO rebase_commits (workflow_run_id, step_id, ordinal, orig_sha, author, subject, kind, pr_number, patch_id,
				detail, status, new_sha, resolution_id, resolution_source, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		`, runID, stepID, c.Ordinal, c.OrigSHA, c.Author, c.Subject, c.Kind, c.PR, c.PatchID,
			c.Detail, c.Status, c.NewSHA, c.ResolutionID, c.ResolutionSource, c.Note); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// UpdateCommit writes one row's mutable fields.
func (s *Store) UpdateCommit(ctx context.Context, runID, stepID string, c CommitRow) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE rebase_commits SET status=$4, new_sha=$5, resolution_id=$6, resolution_source=$7, note=$8
		 WHERE workflow_run_id=$1 AND step_id=$2 AND ordinal=$3
	`, runID, stepID, c.Ordinal, c.Status, c.NewSHA, c.ResolutionID, c.ResolutionSource, c.Note)
	return err
}

// ListCommits returns a run's commit table in ordinal order.
func (s *Store) ListCommits(ctx context.Context, runID, stepID string) ([]CommitRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ordinal, orig_sha, author, subject, kind, pr_number, patch_id, detail, status, new_sha,
		       resolution_id, resolution_source, note
		  FROM rebase_commits WHERE workflow_run_id=$1 AND step_id=$2 ORDER BY ordinal
	`, runID, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CommitRow{}
	for rows.Next() {
		var c CommitRow
		if err := rows.Scan(&c.Ordinal, &c.OrigSHA, &c.Author, &c.Subject, &c.Kind, &c.PR, &c.PatchID, &c.Detail, &c.Status,
			&c.NewSHA, &c.ResolutionID, &c.ResolutionSource, &c.Note); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── conflicts ─────────────────────────────────────────────────────────────────

// Conflict is a captured cherry-pick conflict.
type Conflict struct {
	ConflictID    string          `json:"conflict_id"`
	WorkflowRunID string          `json:"workflow_run_id"`
	StepID        string          `json:"step_id"`
	WorkflowID    string          `json:"workflow_id"`
	Ordinal       int             `json:"ordinal"`
	OrigSHA       string          `json:"orig_sha"`
	Subject       string          `json:"subject"`
	Fingerprint   string          `json:"fingerprint"`
	Files         json.RawMessage `json:"files"`
	Status        string          `json:"status"` // open | resolved
	CreatedAt     time.Time       `json:"created_at"`
	ResolvedAt    *time.Time      `json:"resolved_at,omitempty"`
}

// InsertConflict stores a new open conflict.
func (s *Store) InsertConflict(ctx context.Context, c Conflict) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conflicts (conflict_id, workflow_run_id, step_id, workflow_id, ordinal, orig_sha, subject, fingerprint, files)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, c.ConflictID, c.WorkflowRunID, c.StepID, c.WorkflowID, c.Ordinal, c.OrigSHA, c.Subject, c.Fingerprint, []byte(c.Files))
	return err
}

// GetConflict returns a conflict, or nil.
func (s *Store) GetConflict(ctx context.Context, id string) (*Conflict, error) {
	var c Conflict
	var files []byte
	err := s.pool.QueryRow(ctx, `
		SELECT conflict_id, workflow_run_id, step_id, workflow_id, ordinal, orig_sha, subject, fingerprint, files, status, created_at, resolved_at
		  FROM conflicts WHERE conflict_id=$1
	`, id).Scan(&c.ConflictID, &c.WorkflowRunID, &c.StepID, &c.WorkflowID, &c.Ordinal, &c.OrigSHA, &c.Subject, &c.Fingerprint, &files, &c.Status, &c.CreatedAt, &c.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Files = json.RawMessage(files)
	return &c, nil
}

// OpenConflictID returns the id of the run's open conflict, or "".
func (s *Store) OpenConflictID(ctx context.Context, runID, stepID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT conflict_id FROM conflicts WHERE workflow_run_id=$1 AND step_id=$2 AND status='open'
		 ORDER BY created_at DESC LIMIT 1
	`, runID, stepID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CloseConflicts marks every open conflict of the run with this
// fingerprint resolved (a replay may have re-captured the same one).
func (s *Store) CloseConflicts(ctx context.Context, runID, stepID, fingerprint string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE conflicts SET status='resolved', resolved_at=now()
		 WHERE workflow_run_id=$1 AND step_id=$2 AND fingerprint=$3 AND status='open'
	`, runID, stepID, fingerprint)
	return err
}

// ── resolutions ───────────────────────────────────────────────────────────────

// Resolution is a saved answer to a conflict.
type Resolution struct {
	ResolutionID string          `json:"resolution_id"`
	WorkflowID   string          `json:"workflow_id"`
	StepID       string          `json:"step_id"`
	Fingerprint  string          `json:"fingerprint"`
	Subject      string          `json:"subject"`
	OrigSHA      string          `json:"orig_sha"`
	Kind         string          `json:"kind"` // files | skip
	Files        json.RawMessage `json:"files"`
	Reason       string          `json:"reason"`
	ResolvedBy   string          `json:"resolved_by"`
	CreatedInRun string          `json:"created_in_run"`
	UseCount     int             `json:"use_count"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	LastUsedAt   *time.Time      `json:"last_used_at,omitempty"`
}

const resCols = `resolution_id, workflow_id, step_id, fingerprint, subject, orig_sha, kind, files, reason, resolved_by,
	created_in_run, use_count, created_at, updated_at, last_used_at`

func scanResolution(row pgx.Row) (*Resolution, error) {
	var r Resolution
	var files []byte
	err := row.Scan(&r.ResolutionID, &r.WorkflowID, &r.StepID, &r.Fingerprint, &r.Subject, &r.OrigSHA, &r.Kind, &files, &r.Reason,
		&r.ResolvedBy, &r.CreatedInRun, &r.UseCount, &r.CreatedAt, &r.UpdatedAt, &r.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Files = json.RawMessage(files)
	return &r, nil
}

// UpsertResolution saves (or replaces) the resolution for a
// fingerprint and returns the stored row.
func (s *Store) UpsertResolution(ctx context.Context, r Resolution) (*Resolution, error) {
	if r.ResolutionID == "" {
		r.ResolutionID = NewID("r_")
	}
	if len(r.Files) == 0 {
		r.Files = json.RawMessage(`{}`)
	}
	return scanResolution(s.pool.QueryRow(ctx, `
		INSERT INTO resolutions (resolution_id, workflow_id, step_id, fingerprint, subject, orig_sha, kind, files, reason, resolved_by, created_in_run)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (workflow_id, step_id, fingerprint) DO UPDATE SET
			subject=EXCLUDED.subject, orig_sha=EXCLUDED.orig_sha, kind=EXCLUDED.kind, files=EXCLUDED.files,
			reason=EXCLUDED.reason, resolved_by=EXCLUDED.resolved_by, created_in_run=EXCLUDED.created_in_run, updated_at=now()
		RETURNING `+resCols,
		r.ResolutionID, r.WorkflowID, r.StepID, r.Fingerprint, r.Subject, r.OrigSHA, r.Kind, []byte(r.Files), r.Reason, r.ResolvedBy, r.CreatedInRun))
}

// FindResolution returns the exact-fingerprint resolution, or nil.
func (s *Store) FindResolution(ctx context.Context, workflowID, stepID, fingerprint string) (*Resolution, error) {
	return scanResolution(s.pool.QueryRow(ctx, `SELECT `+resCols+` FROM resolutions WHERE workflow_id=$1 AND step_id=$2 AND fingerprint=$3`,
		workflowID, stepID, fingerprint))
}

// TouchResolution records a reuse.
func (s *Store) TouchResolution(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE resolutions SET use_count = use_count + 1, last_used_at = now() WHERE resolution_id=$1`, id)
	return err
}

// ListResolutions lists a step's saved resolutions, newest first. If
// subject is non-empty only resolutions for that commit subject are
// returned (hints for a new conflict on the same carry).
func (s *Store) ListResolutions(ctx context.Context, workflowID, stepID, subject string) ([]Resolution, error) {
	q := `SELECT ` + resCols + ` FROM resolutions WHERE workflow_id=$1 AND step_id=$2`
	args := []any{workflowID, stepID}
	if subject != "" {
		q += ` AND subject=$3`
		args = append(args, subject)
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Resolution{}
	for rows.Next() {
		r, err := scanResolution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// DeleteResolutions removes saved resolutions of a step together with
// the rerere entries linked to them. Returns (resolutions, rerere
// entries) deleted.
func (s *Store) DeleteResolutions(ctx context.Context, workflowID, stepID string, ids []string) (int64, int64, error) {
	if len(ids) == 0 {
		return 0, 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rr, err := tx.Exec(ctx, `DELETE FROM rerere_entries WHERE workflow_id=$1 AND step_id=$2 AND resolution_id = ANY($3)`, workflowID, stepID, ids)
	if err != nil {
		return 0, 0, err
	}
	res, err := tx.Exec(ctx, `DELETE FROM resolutions WHERE workflow_id=$1 AND step_id=$2 AND resolution_id = ANY($3)`, workflowID, stepID, ids)
	if err != nil {
		return 0, 0, err
	}
	return res.RowsAffected(), rr.RowsAffected(), tx.Commit(ctx)
}

// ── decisions ─────────────────────────────────────────────────────────────────

// Decision answers a question about one fork commit.
type Decision struct {
	WorkflowID string    `json:"workflow_id"`
	StepID     string    `json:"step_id"`
	OrigSHA    string    `json:"orig_sha"`
	Question   string    `json:"question"` // classify | empty
	Decision   string    `json:"decision"` // carry | drop | keep_empty
	Reason     string    `json:"reason"`
	Subject    string    `json:"subject"`
	DecidedBy  string    `json:"decided_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// UpsertDecision saves a decision.
func (s *Store) UpsertDecision(ctx context.Context, d Decision) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO decisions (workflow_id, step_id, orig_sha, question, decision, reason, subject, decided_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (workflow_id, step_id, orig_sha, question) DO UPDATE SET
			decision=EXCLUDED.decision, reason=EXCLUDED.reason, subject=EXCLUDED.subject,
			decided_by=EXCLUDED.decided_by, created_at=now()
	`, d.WorkflowID, d.StepID, d.OrigSHA, d.Question, d.Decision, d.Reason, d.Subject, d.DecidedBy)
	return err
}

// ListDecisions lists a step's decisions keyed "<orig_sha>/<question>".
func (s *Store) ListDecisions(ctx context.Context, workflowID, stepID string) ([]Decision, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT workflow_id, step_id, orig_sha, question, decision, reason, subject, decided_by, created_at
		  FROM decisions WHERE workflow_id=$1 AND step_id=$2 ORDER BY created_at DESC
	`, workflowID, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Decision{}
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.WorkflowID, &d.StepID, &d.OrigSHA, &d.Question, &d.Decision, &d.Reason, &d.Subject, &d.DecidedBy, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDecision removes one decision.
func (s *Store) DeleteDecision(ctx context.Context, workflowID, stepID, origSHA, question string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM decisions WHERE workflow_id=$1 AND step_id=$2 AND orig_sha=$3 AND question=$4`,
		workflowID, stepID, origSHA, question)
	return err
}

// ── rerere ────────────────────────────────────────────────────────────────────

// RerereEntry mirrors gitrebase.RerereEntry for storage, plus what
// produced it: the saved resolution (empty when rerere resolved it on
// its own), the commit subject, and the conflicted path. All three are
// empty when git's MERGE_RR didn't name the entry at export time.
type RerereEntry struct {
	ID           string            `json:"id"`
	Files        map[string]string `json:"files,omitempty"`
	ResolutionID string            `json:"resolution_id,omitempty"`
	Subject      string            `json:"subject,omitempty"`
	Path         string            `json:"path,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// ListRerere returns a step's stored rerere cache.
func (s *Store) ListRerere(ctx context.Context, workflowID, stepID string) ([]RerereEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT entry_id, files FROM rerere_entries WHERE workflow_id=$1 AND step_id=$2`, workflowID, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RerereEntry
	for rows.Next() {
		var e RerereEntry
		var files []byte
		if err := rows.Scan(&e.ID, &files); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(files, &e.Files); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpsertRerere stores rerere cache entries. Link fields are only
// written when provided, so a later export that doesn't know an
// entry's origin never erases it.
func (s *Store) UpsertRerere(ctx context.Context, workflowID, stepID string, entries []RerereEntry) error {
	for _, e := range entries {
		files, err := json.Marshal(e.Files)
		if err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO rerere_entries (workflow_id, step_id, entry_id, files, resolution_id, subject, path)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (workflow_id, step_id, entry_id) DO UPDATE SET
				files         = EXCLUDED.files,
				resolution_id = CASE WHEN EXCLUDED.resolution_id <> '' THEN EXCLUDED.resolution_id ELSE rerere_entries.resolution_id END,
				subject       = CASE WHEN EXCLUDED.subject <> '' THEN EXCLUDED.subject ELSE rerere_entries.subject END,
				path          = CASE WHEN EXCLUDED.path <> '' THEN EXCLUDED.path ELSE rerere_entries.path END,
				updated_at    = now()
		`, workflowID, stepID, e.ID, files, e.ResolutionID, e.Subject, e.Path); err != nil {
			return err
		}
	}
	return nil
}

// ListRerereMeta lists a step's rerere entries without their contents.
func (s *Store) ListRerereMeta(ctx context.Context, workflowID, stepID string) ([]RerereEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT entry_id, resolution_id, subject, path, updated_at
		  FROM rerere_entries WHERE workflow_id=$1 AND step_id=$2 ORDER BY updated_at DESC
	`, workflowID, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RerereEntry{}
	for rows.Next() {
		var e RerereEntry
		if err := rows.Scan(&e.ID, &e.ResolutionID, &e.Subject, &e.Path, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteRerere removes rerere entries of a step by id.
func (s *Store) DeleteRerere(ctx context.Context, workflowID, stepID string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM rerere_entries WHERE workflow_id=$1 AND step_id=$2 AND entry_id = ANY($3)`, workflowID, stepID, ids)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DecisionKey identifies one decision.
type DecisionKey struct {
	OrigSHA  string `json:"orig_sha"`
	Question string `json:"question"`
}

// DeleteDecisions removes decisions of a step.
func (s *Store) DeleteDecisions(ctx context.Context, workflowID, stepID string, keys []DecisionKey) (int64, error) {
	var n int64
	for _, k := range keys {
		tag, err := s.pool.Exec(ctx, `DELETE FROM decisions WHERE workflow_id=$1 AND step_id=$2 AND orig_sha=$3 AND question=$4`,
			workflowID, stepID, k.OrigSHA, k.Question)
		if err != nil {
			return n, err
		}
		n += tag.RowsAffected()
	}
	return n, nil
}

// WipeOptions selects what Wipe deletes.
type WipeOptions struct {
	Resolutions bool `json:"resolutions"`
	Decisions   bool `json:"decisions"`
	Rerere      bool `json:"rerere"`
	Patches     bool `json:"patches"`
	RunHistory  bool `json:"run_history"`
}

// Wipe deletes a workflow's memory in one transaction. stepID scopes it
// to one step; "" wipes every step of the workflow. Deleting
// resolutions always deletes their linked rerere entries. Run history
// only covers finished runs - running / suspended runs keep their
// commit table so they can still resume. Returns rows deleted per
// category.
func (s *Store) Wipe(ctx context.Context, workflowID, stepID string, o WipeOptions) (map[string]int64, error) {
	if workflowID == "" {
		return nil, errors.New("workflow_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	counts := map[string]int64{}
	// $2 = '' matches every step.
	scope := `workflow_id = $1 AND ($2 = '' OR step_id = $2)`
	exec := func(key, q string) error {
		tag, err := tx.Exec(ctx, q, workflowID, stepID)
		if err != nil {
			return fmt.Errorf("wipe %s: %w", key, err)
		}
		counts[key] += tag.RowsAffected()
		return nil
	}
	if o.Rerere {
		if err := exec("rerere", `DELETE FROM rerere_entries WHERE `+scope); err != nil {
			return nil, err
		}
	}
	if o.Resolutions {
		if err := exec("rerere", `DELETE FROM rerere_entries WHERE `+scope+` AND resolution_id <> ''`); err != nil {
			return nil, err
		}
		if err := exec("resolutions", `DELETE FROM resolutions WHERE `+scope); err != nil {
			return nil, err
		}
	}
	if o.Decisions {
		if err := exec("decisions", `DELETE FROM decisions WHERE `+scope); err != nil {
			return nil, err
		}
	}
	if o.Patches {
		if err := exec("patches", `DELETE FROM step_patches WHERE `+scope); err != nil {
			return nil, err
		}
	}
	if o.RunHistory {
		finished := `SELECT workflow_run_id, step_id FROM rebase_runs WHERE ` + scope + ` AND status NOT IN ('running', 'suspended')`
		if err := exec("run_history_commits", `DELETE FROM rebase_commits WHERE (workflow_run_id, step_id) IN (`+finished+`)`); err != nil {
			return nil, err
		}
		if err := exec("run_history_conflicts", `DELETE FROM conflicts WHERE (workflow_run_id, step_id) IN (`+finished+`)`); err != nil {
			return nil, err
		}
		if err := exec("run_history", `DELETE FROM rebase_runs WHERE `+scope+` AND status NOT IN ('running', 'suspended')`); err != nil {
			return nil, err
		}
	}
	return counts, tx.Commit(ctx)
}

// RunKey identifies one rebase run record.
type RunKey struct {
	WorkflowRunID string
	StepID        string
}

// ListRunKeys returns every run record of a workflow, any status.
func (s *Store) ListRunKeys(ctx context.Context, workflowID string) ([]RunKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT workflow_run_id, step_id FROM rebase_runs WHERE workflow_id=$1`, workflowID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RunKey, error) {
		var k RunKey
		err := row.Scan(&k.WorkflowRunID, &k.StepID)
		return k, err
	})
}

// PurgeWorkflow deletes everything stored for a workflow, in one
// transaction: every step's memory (resolutions, decisions, rerere
// cache, patch queue) and every run's records (run rows, commit tables,
// conflicts, command history) regardless of status. Only for
// Plugin.PurgeWorkflow, which the orchestrator calls once none of the
// workflow's runs is in flight. Returns rows deleted per category.
func (s *Store) PurgeWorkflow(ctx context.Context, workflowID string) (map[string]int64, error) {
	if workflowID == "" {
		return nil, errors.New("workflow_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runs := `SELECT workflow_run_id, step_id FROM rebase_runs WHERE workflow_id = $1`
	counts := map[string]int64{}
	// rebase_commits has no workflow_id: delete through rebase_runs
	// before the run rows go.
	for _, q := range []struct{ key, sql string }{
		{"commits", `DELETE FROM rebase_commits WHERE (workflow_run_id, step_id) IN (` + runs + `)`},
		{"conflicts", `DELETE FROM conflicts WHERE workflow_id = $1`},
		{"runs", `DELETE FROM rebase_runs WHERE workflow_id = $1`},
		{"resolutions", `DELETE FROM resolutions WHERE workflow_id = $1`},
		{"decisions", `DELETE FROM decisions WHERE workflow_id = $1`},
		{"rerere", `DELETE FROM rerere_entries WHERE workflow_id = $1`},
		{"patches", `DELETE FROM step_patches WHERE workflow_id = $1`},
		{"command_log", `DELETE FROM command_log WHERE workflow_id = $1`},
	} {
		tag, err := tx.Exec(ctx, q.sql, workflowID)
		if err != nil {
			return nil, fmt.Errorf("purge %s: %w", q.key, err)
		}
		counts[q.key] = tag.RowsAffected()
	}
	return counts, tx.Commit(ctx)
}

// CountRerere returns how many rerere entries a step has.
func (s *Store) CountRerere(ctx context.Context, workflowID, stepID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM rerere_entries WHERE workflow_id=$1 AND step_id=$2`, workflowID, stepID).Scan(&n)
	return n, err
}
