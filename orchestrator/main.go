// Orchestrator service: serves the web shell and forwards plugin
// invocations over gRPC. Has no knowledge of what any plugin actually
// does - it only speaks the shared Plugin contract.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jacobsee/upstream-readiness/orchestrator/internal/httpapi"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/registry"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/runbus"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/storage"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/wfengine"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/workflow"
)

//go:embed all:web/dist
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	pluginsCSV := flag.String("plugins", "localhost:9000",
		"comma-separated list of plugin gRPC endpoints")
	workflowsDir := flag.String("workflows", "",
		"directory of workflow files; every *.yaml file in it is loaded on startup and upserted into Postgres.")
	pgDSN := flag.String("pg-dsn", envOr("PG_DSN", "postgres://orchestrator:orchestrator@localhost:5432/orchestrator?sslmode=disable"),
		"Postgres connection string for orchestrator storage")
	temporalAddr := flag.String("temporal", envOr("TEMPORAL_ADDRESS", "localhost:7233"),
		"Temporal frontend address")
	flag.Parse()

	web, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, *pgDSN)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer store.Close()

	reg := registry.New()
	regCtx, regCancel := context.WithTimeout(ctx, 30*time.Second)
	for _, ep := range splitAndTrim(*pluginsCSV) {
		if err := reg.Register(regCtx, ep); err != nil {
			log.Printf("plugin %q: %v (skipping)", ep, err)
			continue
		}
		log.Printf("registered plugin at %s", ep)
	}
	regCancel()

	bus := runbus.New(64)
	engine, err := wfengine.Start(ctx, *temporalAddr, reg, store, bus)
	if err != nil {
		log.Fatalf("temporal: %v", err)
	}
	defer engine.Stop()
	log.Printf("temporal worker started; task queue=%s", wfengine.TaskQueue)

	if *workflowsDir != "" {
		defs, loadErr := workflow.LoadDir(*workflowsDir)
		if loadErr != nil {
			log.Printf("warning: load workflows from %s: %v", *workflowsDir, loadErr)
		} else {
			log.Printf("loaded %d workflow(s) from %s", len(defs), *workflowsDir)
			ids := make([]string, 0, len(defs))
			for _, def := range defs {
				ids = append(ids, def.ID)
				b, _ := json.Marshal(def)
				if err := store.UpsertWorkflow(ctx, storage.WorkflowRecord{
					ID: def.ID, DisplayName: def.DisplayName,
					Schedule: def.Schedule, Definition: b,
				}); err != nil {
					log.Printf("warning: upsert workflow %q: %v", def.ID, err)
				}
			}
			// The directory is the source of truth: a stored workflow
			// whose file is gone is flagged removed (history kept until
			// purged). Only after a complete load - LoadDir fails on any
			// bad file, so a typo can't flag everything.
			removed, err := store.MarkWorkflowsRemovedExcept(ctx, ids)
			if err != nil {
				log.Printf("warning: flag removed workflows: %v", err)
			}
			for _, id := range removed {
				log.Printf("workflow %q no longer has a file in %s; marked removed", id, *workflowsDir)
			}
			if err := engine.EnsureSchedules(ctx, defs); err != nil {
				log.Printf("warning: ensure schedules: %v", err)
			}
		}
	}

	srv := httpapi.New(reg, web, engine, store)
	log.Printf("orchestrator listening on %s", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
