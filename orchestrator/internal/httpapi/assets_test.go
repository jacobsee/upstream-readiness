package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/jacobsee/upstream-readiness/orchestrator/internal/registry"
)

// The asset route must coexist with the shell's "/" file server and
// reject paths outside a plugin's asset root before dialling anything.
func TestPluginAssetRoute(t *testing.T) {
	s := New(registry.New(), fstest.MapFS{"index.html": {Data: []byte("shell")}}, nil, nil)

	for path, want := range map[string]int{
		"/plugins/nope/assets/mfe.js":        http.StatusNotFound,   // unknown plugin
		"/plugins/nope/assets/a/%2e%2e/x.js": http.StatusBadRequest, // escapes the root
		"/plugins/nope/assets/":              http.StatusBadRequest, // no path
		"/":                                  http.StatusOK,         // shell still served
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s: got %d, want %d", path, rec.Code, want)
		}
	}

	if got := pluginAssetURL("greeter", "greeter-mfe.js"); got != "/plugins/greeter/assets/greeter-mfe.js" {
		t.Errorf("pluginAssetURL = %q", got)
	}
}
