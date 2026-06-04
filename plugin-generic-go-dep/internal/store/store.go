// Package store is the generic-go-dep plugin's own persistence layer.
// Per the load-bearing rules in AGENTS.md, plugins own their own
// database - nothing here ever leaves the plugin, and the orchestrator
// has no direct handle to this schema.
//
// Tables:
//   - step_patches (schema and queries shared via pluginkit/patchqueue).
//     Each row is a git patch a workflow author attached to a
//     (workflow_id, step_id) tuple via the workflow-step-config MFE. On
//     Execute, handleUpdate reads the rows whose target_dir matches the
//     current dir and applies them in ordinal order right after
//     `go mod vendor` and before `make`.
//   - command_log (pluginkit/cmdlog): every command / query / note each
//     update ran, with full output, per (workflow_run_id, step_id).
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jacobsee/upstream-readiness/pluginkit/cmdlog"
	"github.com/jacobsee/upstream-readiness/pluginkit/patchqueue"
)

type Store struct {
	pool *pgxpool.Pool
	// Patches is the per-(workflow_id, step_id) patch queue.
	Patches *patchqueue.Store
	// Commands is every update's complete command history.
	Commands *cmdlog.Store
}

// Open dials Postgres and applies migrations. dsn is a libpq-style
// connection string, e.g. postgres://godep:godep@localhost:5433/godep.
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
		if _, err := s.pool.Exec(ctx, patchqueue.TableSQL); err != nil {
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
