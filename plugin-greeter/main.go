// Greeter plugin service. Runs a single gRPC server implementing the
// Plugin contract, including GetAsset, which serves the plugin's own
// micro-frontend bundle. The browser fetches that bundle through the
// orchestrator, so the plugin never needs to be reachable from it.
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net"

	"github.com/jacobsee/upstream-readiness/plugin-greeter/internal/service"
	"github.com/jacobsee/upstream-readiness/pluginkit/mfeassets"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc"
)

// Vite builds the React MFE into web/dist/greeter-mfe.js. Build it
// before compiling the plugin binary (see README).
//
//go:embed all:web/dist
var webFS embed.FS

func main() {
	grpcAddr := flag.String("grpc-addr", ":9000", "gRPC listen address (orchestrator dials this)")
	flag.Parse()

	web, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}
	assets, err := mfeassets.New(web, "greeter-mfe.js")
	if err != nil {
		log.Fatalf("assets: %v", err)
	}

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServer(srv, &service.Greeter{Assets: assets})
	log.Printf("greeter gRPC listening on %s", *grpcAddr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
