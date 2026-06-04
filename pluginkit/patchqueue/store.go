// Package patchqueue is the per-(workflow_id, step_id) queue of
// user-defined git patches that the repo's Go plugins re-apply on every
// run of a step: storage (against the plugin's own Postgres), the
// patches_* MFE-backing action handlers, and ApplyPatch.
//
// Each plugin owns its database (load-bearing rule: plugins own their
// own state). This package never opens a connection - the plugin hands
// in the pool for its own DB and includes TableSQL in its own versioned
// migrations.
package patchqueue

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TableSQL creates the step_patches table. Idempotent. Plugins run it
// inside their own schema_version-gated migration block.
const TableSQL = `
	CREATE TABLE IF NOT EXISTS step_patches (
		workflow_id   TEXT NOT NULL,
		step_id       TEXT NOT NULL,
		patch_id      TEXT NOT NULL,
		name          TEXT NOT NULL,
		target_dir    TEXT NOT NULL,
		content       TEXT NOT NULL,
		ordinal       INT  NOT NULL,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (workflow_id, step_id, patch_id)
	);
	CREATE INDEX IF NOT EXISTS step_patches_order_idx
	    ON step_patches (workflow_id, step_id, ordinal);
`

// Store reads and writes step_patches on the plugin's own pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pool whose database already has TableSQL applied.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Patch is one attached git patch. Content is the raw text of a
// git-format-patch / diff blob; ApplyPatch applies it verbatim with
// `git am` (mbox) or `git apply` + `git commit` (plain diff).
type Patch struct {
	WorkflowID string    `json:"workflow_id"`
	StepID     string    `json:"step_id"`
	PatchID    string    `json:"patch_id"`
	Name       string    `json:"name"`
	TargetDir  string    `json:"target_dir"`
	Content    string    `json:"content"`
	Ordinal    int       `json:"ordinal"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ListPatches returns every patch for a (workflow_id, step_id) in
// ordinal order. Ties broken by patch_id so the sequence is stable.
func (s *Store) ListPatches(ctx context.Context, workflowID, stepID string) ([]Patch, error) {
	if workflowID == "" || stepID == "" {
		return nil, errors.New("workflow_id and step_id are required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT workflow_id, step_id, patch_id, name, target_dir, content, ordinal, created_at, updated_at
		  FROM step_patches
		 WHERE workflow_id = $1 AND step_id = $2
		 ORDER BY ordinal ASC, patch_id ASC
	`, workflowID, stepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Patch{}
	for rows.Next() {
		var p Patch
		if err := rows.Scan(&p.WorkflowID, &p.StepID, &p.PatchID, &p.Name, &p.TargetDir, &p.Content, &p.Ordinal, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertPatch creates or replaces a patch. An unset (0) ordinal means
// "don't care": a new row lands at the bottom of the queue (max + 1) and
// an existing row keeps its position. A non-zero ordinal is written as
// given.
func (s *Store) UpsertPatch(ctx context.Context, p Patch) (*Patch, error) {
	if p.WorkflowID == "" || p.StepID == "" || p.PatchID == "" {
		return nil, errors.New("workflow_id, step_id, and patch_id are required")
	}
	if p.Name == "" {
		return nil, errors.New("name is required")
	}
	if p.TargetDir == "" {
		return nil, errors.New("target_dir is required")
	}
	if p.Content == "" {
		return nil, errors.New("content is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// No ordinal from the caller: an edit keeps the patch where it is
	// (the editor never sends one, and writing 0 would move the patch to
	// the front); a new patch is parked at the end so authors don't have
	// to reason about slot numbers just to add one.
	if p.Ordinal == 0 {
		err := tx.QueryRow(ctx, `
			SELECT ordinal FROM step_patches WHERE workflow_id=$1 AND step_id=$2 AND patch_id=$3
		`, p.WorkflowID, p.StepID, p.PatchID).Scan(&p.Ordinal)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			var maxOrd *int
			if err := tx.QueryRow(ctx, `
				SELECT MAX(ordinal) FROM step_patches WHERE workflow_id=$1 AND step_id=$2
			`, p.WorkflowID, p.StepID).Scan(&maxOrd); err != nil {
				return nil, err
			}
			if maxOrd == nil {
				p.Ordinal = 1
			} else {
				p.Ordinal = *maxOrd + 1
			}
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO step_patches (workflow_id, step_id, patch_id, name, target_dir, content, ordinal)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (workflow_id, step_id, patch_id) DO UPDATE SET
			name       = EXCLUDED.name,
			target_dir = EXCLUDED.target_dir,
			content    = EXCLUDED.content,
			ordinal    = EXCLUDED.ordinal,
			updated_at = now()
	`, p.WorkflowID, p.StepID, p.PatchID, p.Name, p.TargetDir, p.Content, p.Ordinal); err != nil {
		return nil, err
	}

	row := tx.QueryRow(ctx, `
		SELECT workflow_id, step_id, patch_id, name, target_dir, content, ordinal, created_at, updated_at
		  FROM step_patches
		 WHERE workflow_id=$1 AND step_id=$2 AND patch_id=$3
	`, p.WorkflowID, p.StepID, p.PatchID)
	var saved Patch
	if err := row.Scan(&saved.WorkflowID, &saved.StepID, &saved.PatchID, &saved.Name, &saved.TargetDir, &saved.Content, &saved.Ordinal, &saved.CreatedAt, &saved.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &saved, nil
}

// DeletePatch removes a single patch. Missing patch is not an error.
func (s *Store) DeletePatch(ctx context.Context, workflowID, stepID, patchID string) error {
	if workflowID == "" || stepID == "" || patchID == "" {
		return errors.New("workflow_id, step_id, and patch_id are required")
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM step_patches WHERE workflow_id=$1 AND step_id=$2 AND patch_id=$3
	`, workflowID, stepID, patchID)
	return err
}

// DeleteWorkflow removes every step's queue of a workflow (for
// Plugin.PurgeWorkflow). Returns the number of patches deleted.
func (s *Store) DeleteWorkflow(ctx context.Context, workflowID string) (int64, error) {
	if workflowID == "" {
		return 0, errors.New("workflow_id is required")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM step_patches WHERE workflow_id=$1`, workflowID)
	return tag.RowsAffected(), err
}

// ReorderPatches assigns 1..N ordinals in the order given. Patch IDs
// not passed in are left untouched (and stay at their existing ordinal,
// which may be ≥ N - callers should list first to know the full set).
// Runs inside a transaction so a partial reorder never leaves the queue
// in an inconsistent state.
func (s *Store) ReorderPatches(ctx context.Context, workflowID, stepID string, orderedPatchIDs []string) error {
	if workflowID == "" || stepID == "" {
		return errors.New("workflow_id and step_id are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Two-phase update: first bump every affected row into a negative
	// range, then assign the final 1..N, so a subsequent SELECT ORDER BY
	// ordinal sees a clean sequence with no lingering artifacts from
	// prior orderings.
	for i, pid := range orderedPatchIDs {
		if _, err := tx.Exec(ctx, `
			UPDATE step_patches SET ordinal = -1 - $4, updated_at = now()
			 WHERE workflow_id=$1 AND step_id=$2 AND patch_id=$3
		`, workflowID, stepID, pid, i); err != nil {
			return err
		}
	}
	for i, pid := range orderedPatchIDs {
		if _, err := tx.Exec(ctx, `
			UPDATE step_patches SET ordinal = $4, updated_at = now()
			 WHERE workflow_id=$1 AND step_id=$2 AND patch_id=$3
		`, workflowID, stepID, pid, i+1); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
