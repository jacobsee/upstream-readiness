// upstream-rebase plugin: rebases a fork onto a new upstream release by
// `merge -s ours` + re-picking every UPSTREAM carry / PR commit since the
// previous rebase, pausing for human conflict resolution (stored and
// reused across runs), then running the shared dependency / patch-queue
// / make pipeline and pushing to a fork. Exposes a gRPC Plugin service,
// which also serves its micro-frontend bundle (GetAsset) for the
// orchestrator to proxy to the browser.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/service"
	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/mfeassets"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc"
)

//go:embed all:web/dist
var webFS embed.FS

// workDirTTL bounds how long a suspended step's working copy is kept.
// Past it, a resume simply replays from scratch.
const workDirTTL = 7 * 24 * time.Hour

func main() {
	grpcAddr := flag.String("grpc-addr", ":9200", "gRPC listen address")
	pgDSN := flag.String("pg-dsn", "", "Postgres DSN for the plugin's own store (default env PG_DSN, then localhost:5434)")
	workDir := flag.String("work-dir", "", "Where suspended rebases keep their working copies (default env REBASE_WORK_DIR, then $TMPDIR/upstream-rebase)")
	flag.Parse()

	dsn := *pgDSN
	if dsn == "" {
		dsn = os.Getenv("PG_DSN")
	}
	if dsn == "" {
		dsn = "postgres://rebase:rebase@localhost:5434/rebase?sslmode=disable"
	}
	root := *workDir
	if root == "" {
		root = os.Getenv("REBASE_WORK_DIR")
	}
	if root == "" {
		root = filepath.Join(os.TempDir(), "upstream-rebase")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		log.Fatalf("work dir: %v", err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("open plugin store: %v", err)
	}
	defer st.Close()

	web, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}
	assets, err := mfeassets.New(web, "upstream-rebase-mfe.js")
	if err != nil {
		log.Fatalf("assets: %v", err)
	}

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	rebaser := &service.Rebaser{Assets: assets, Store: st, WorkRoot: root}
	log.Printf("upstream-rebase work dir %s", root)
	// Before serving: nothing is running yet, so every working copy that
	// isn't a recently-suspended run's resume cache is an orphan (killed
	// mid-run, ReleaseStep missed, or past the TTL).
	removed, err := rebaser.SweepWorkRoot(ctx, workDirTTL)
	for _, d := range removed {
		log.Printf("work dir sweep: removed %s", d)
	}
	if err != nil {
		log.Printf("work dir sweep: %v", err)
	}

	srv := grpc.NewServer()
	pluginpb.RegisterPluginServer(srv, rebaser)
	log.Printf("upstream-rebase gRPC listening on %s (work dir %s)", *grpcAddr, root)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
