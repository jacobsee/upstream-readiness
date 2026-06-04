// generic-go-dep plugin: clones a Go repository, updates one module
// dependency to a specified version, runs go mod tidy and optional make
// targets, then pushes a new branch. Exposes a gRPC Plugin service,
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

	"github.com/jacobsee/upstream-readiness/plugin-generic-go-dep/internal/service"
	"github.com/jacobsee/upstream-readiness/plugin-generic-go-dep/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/mfeassets"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc"
)

//go:embed all:web/dist
var webFS embed.FS

func main() {
	grpcAddr := flag.String("grpc-addr", ":9100", "gRPC listen address")
	pgDSN := flag.String("pg-dsn", "", "Postgres DSN for the plugin's own store (default env PG_DSN, then localhost)")
	workDir := flag.String("work-dir", "", "Where in-flight updates keep their clones; emptied at startup (default env GODEP_WORK_DIR, then $TMPDIR/generic-go-dep)")
	flag.Parse()

	dsn := *pgDSN
	if dsn == "" {
		dsn = os.Getenv("PG_DSN")
	}
	if dsn == "" {
		dsn = "postgres://godep:godep@localhost:5433/godep?sslmode=disable"
	}
	root := *workDir
	if root == "" {
		root = os.Getenv("GODEP_WORK_DIR")
	}
	if root == "" {
		root = filepath.Join(os.TempDir(), "generic-go-dep")
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
	assets, err := mfeassets.New(web, "go-dep-mfe.js")
	if err != nil {
		log.Fatalf("assets: %v", err)
	}

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	updater := &service.GoDepUpdater{Assets: assets, Store: st, WorkRoot: root}
	log.Printf("generic-go-dep work dir %s", root)
	// Before serving: no update is running yet, so anything here was left
	// by a process that died mid-run.
	removed, err := updater.SweepWorkRoot()
	for _, d := range removed {
		log.Printf("work dir sweep: removed %s", d)
	}
	if err != nil {
		log.Printf("work dir sweep: %v", err)
	}

	srv := grpc.NewServer()
	pluginpb.RegisterPluginServer(srv, updater)
	log.Printf("generic-go-dep gRPC listening on %s (work dir %s)", *grpcAddr, root)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
