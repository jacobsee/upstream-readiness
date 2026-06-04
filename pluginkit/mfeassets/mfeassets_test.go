package mfeassets

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"

	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeStream collects what GetAsset sends.
type fakeStream struct {
	grpc.ServerStream
	got []*pluginpb.AssetChunk
}

func (f *fakeStream) Send(c *pluginpb.AssetChunk) error {
	f.got = append(f.got, c)
	return nil
}

func TestGetAsset(t *testing.T) {
	big := strings.Repeat("x", chunkSize*2+10)
	s, err := New(fstest.MapFS{
		"mfe.js":         {Data: []byte(big)},
		"styles/app.css": {Data: []byte("body{}")},
	}, "mfe.js")
	if err != nil {
		t.Fatal(err)
	}

	st := &fakeStream{}
	if err := s.GetAsset(&pluginpb.GetAssetRequest{Path: "mfe.js"}, st); err != nil {
		t.Fatal(err)
	}
	h := st.got[0].GetHeader()
	if h == nil || h.GetContentType() != "text/javascript; charset=utf-8" || h.GetEtag() == "" {
		t.Fatalf("bad header: %v", st.got[0])
	}
	var body bytes.Buffer
	for _, c := range st.got[1:] {
		if c.GetHeader() != nil {
			t.Fatal("second header")
		}
		body.Write(c.GetData())
	}
	if body.String() != big || len(st.got) != 4 {
		t.Fatalf("got %d bytes in %d messages", body.Len(), len(st.got))
	}

	st = &fakeStream{}
	if err := s.GetAsset(&pluginpb.GetAssetRequest{Path: "mfe.js", IfNoneMatch: h.GetEtag()}, st); err != nil {
		t.Fatal(err)
	}
	if len(st.got) != 1 || !st.got[0].GetHeader().GetNotModified() {
		t.Fatalf("expected a lone not-modified header, got %v", st.got)
	}

	for path, want := range map[string]codes.Code{
		"missing.js":     codes.NotFound,
		"../go.mod":      codes.InvalidArgument,
		"/mfe.js":        codes.InvalidArgument,
		"":               codes.InvalidArgument,
		"styles":         codes.NotFound,
		"styles/app.css": codes.OK,
	} {
		err := s.GetAsset(&pluginpb.GetAssetRequest{Path: path}, &fakeStream{})
		if got := status.Code(err); got != want {
			t.Errorf("%q: got %v, want %v", path, got, want)
		}
	}
}

func TestNewRequiresEntry(t *testing.T) {
	if _, err := New(fstest.MapFS{"other.js": {}}, "mfe.js"); err == nil {
		t.Fatal("expected error for missing entry")
	}
}
