package gitenv

import (
	"strings"
	"testing"
)

func envValue(env []string, key string) (string, int) {
	val, n := "", 0
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val, n = v, n+1
		}
	}
	return val, n
}

func TestBuildGitEnvRunInputs(t *testing.T) {
	dir := t.TempDir()
	cfg := InstanceConfig{EnvVars: map[string]string{"VERSION": "from-instance", "OTHER": "kept"}}
	env, err := BuildGitEnv(dir, dir+"/gopath", cfg, nil, map[string]string{"VERSION": "1.2", "EXTRA": ""})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"VERSION": "1.2", "OTHER": "kept", "EXTRA": "", "HOME": dir} {
		got, n := envValue(env, key)
		if n != 1 || got != want {
			t.Errorf("%s = %q (%d entries), want %q once", key, got, n, want)
		}
	}
}

func TestBuildGitEnvRejectsProtectedRunInput(t *testing.T) {
	dir := t.TempDir()
	_, err := BuildGitEnv(dir, dir+"/gopath", InstanceConfig{}, nil, map[string]string{"GOPATH": "/x"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want reserved-key error", err)
	}
}
