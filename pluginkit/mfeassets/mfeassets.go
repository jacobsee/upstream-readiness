// Package mfeassets serves a plugin's embedded micro-frontend bundle over
// the Plugin.GetAsset RPC. The browser never reaches a plugin directly:
// the orchestrator exposes /plugins/{id}/assets/{path} on its own origin
// and streams these bytes through, so a plugin needs exactly one
// listener (gRPC) and no knowledge of any public URL.
package mfeassets

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"path"

	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// chunkSize keeps every AssetChunk well under gRPC's default 4 MiB
// message limit regardless of bundle size.
const chunkSize = 64 << 10

// Server answers GetAsset from a read-only filesystem (normally the
// plugin's //go:embed'd web/dist). Contents are hashed once at
// construction, since an embedded FS can't change at runtime.
type Server struct {
	fsys  fs.FS
	entry string
	etags map[string]string
}

// New indexes fsys and checks that entry (the MFE module path advertised
// as PluginManifest.mfe_entry) exists, so a plugin built without its
// web/dist fails at startup rather than in the browser.
func New(fsys fs.FS, entry string) (*Server, error) {
	s := &Server{fsys: fsys, entry: entry, etags: map[string]string{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		s.etags[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index MFE assets: %w", err)
	}
	if _, ok := s.etags[entry]; !ok {
		return nil, fmt.Errorf("MFE entry %q not found in assets (was web/dist built?)", entry)
	}
	return s, nil
}

// Entry is the value for PluginManifest.mfe_entry.
func (s *Server) Entry() string { return s.entry }

// GetAsset implements Plugin.GetAsset.
func (s *Server) GetAsset(req *pluginpb.GetAssetRequest, stream grpc.ServerStreamingServer[pluginpb.AssetChunk]) error {
	p := req.GetPath()
	// fs.ValidPath rejects "..", leading slashes, and empty elements, so
	// nothing outside the asset root is addressable.
	if !fs.ValidPath(p) || p == "." {
		return status.Errorf(codes.InvalidArgument, "invalid asset path %q", p)
	}
	etag, ok := s.etags[p]
	if !ok {
		return status.Errorf(codes.NotFound, "asset %q not found", p)
	}
	header := &pluginpb.AssetHeader{ContentType: contentType(p), Etag: etag}
	if req.GetIfNoneMatch() == etag {
		header.NotModified = true
		return stream.Send(&pluginpb.AssetChunk{Part: &pluginpb.AssetChunk_Header{Header: header}})
	}
	b, err := fs.ReadFile(s.fsys, p)
	if err != nil {
		return status.Errorf(codes.Internal, "read asset %q: %v", p, err)
	}
	if err := stream.Send(&pluginpb.AssetChunk{Part: &pluginpb.AssetChunk_Header{Header: header}}); err != nil {
		return err
	}
	for len(b) > 0 {
		n := min(len(b), chunkSize)
		if err := stream.Send(&pluginpb.AssetChunk{Part: &pluginpb.AssetChunk_Data{Data: b[:n]}}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func contentType(p string) string {
	switch path.Ext(p) {
	// Spelled out rather than left to the OS mime table: browsers refuse
	// to execute an ES module served with anything but a JS MIME type.
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".map", ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}
