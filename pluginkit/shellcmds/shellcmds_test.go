package shellcmds

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
)

func newRepo(t *testing.T) (*runner.Runner, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+t.TempDir(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	r := runner.New(context.Background(), env, nil)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "base"}} {
		if err := r.Run(repo, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	return r, repo
}

func gitOut(t *testing.T, r *runner.Runner, repo string, args ...string) string {
	t.Helper()
	out, err := r.Output(repo, "git", args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func TestRun(t *testing.T) {
	r, repo := newRepo(t)
	cmds := []Command{
		{Name: "write file", Run: `echo "$((1+1))" > out.txt`},
		{Name: "no-op", Run: "true"},
		{Name: "in sub", Run: "pwd > where.txt", Dir: "sub"},
	}
	if err := Normalize(cmds); err != nil {
		t.Fatal(err)
	}
	msg := func(c Command) string { return "cmd: " + c.Name + "\n\n$ " + c.Run }
	if err := Run(r, repo, cmds, msg); err != nil {
		t.Fatal(err)
	}
	log := gitOut(t, r, repo, "log", "--format=%s")
	if log != "cmd: in sub\ncmd: write file\nbase" {
		t.Errorf("log = %q, want one commit per changing command, none for the no-op", log)
	}
	if body := gitOut(t, r, repo, "log", "-1", "--format=%b", "HEAD~1"); body != `$ echo "$((1+1))" > out.txt` {
		t.Errorf("commit body = %q", body)
	}
	if got := gitOut(t, r, repo, "show", "HEAD~1:out.txt"); got != "2" {
		t.Errorf("out.txt = %q", got)
	}
	where := gitOut(t, r, repo, "show", "HEAD:sub/where.txt")
	if !strings.HasSuffix(where, string(filepath.Separator)+"sub") {
		t.Errorf("command ran in %q, want the sub dir", where)
	}
}

func TestRunFailure(t *testing.T) {
	r, repo := newRepo(t)
	cmds := []Command{{Name: "boom", Run: "exit 3", Dir: "."}}
	err := Run(r, repo, cmds, func(c Command) string { return c.Name })
	if err == nil || !strings.Contains(err.Error(), `commands[0] "boom"`) {
		t.Errorf("err = %v, want a commands[0] failure", err)
	}
}

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		c    Command
		want string
	}{
		{Command{Run: "true"}, "name is required"},
		{Command{Name: "a\nb", Run: "true"}, "single line"},
		{Command{Name: "a", Run: "  "}, "run is required"},
		{Command{Name: "a", Run: "true", Dir: "../x"}, "commands[0].dir"},
	} {
		err := Normalize([]Command{tc.c})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Normalize(%+v) = %v, want %q", tc.c, err, tc.want)
		}
	}
	cmds := []Command{{Name: "a", Run: "true"}, {Name: "b", Run: "true", Dir: "x/./y/"}}
	if err := Normalize(cmds); err != nil {
		t.Fatal(err)
	}
	if cmds[0].Dir != "." || cmds[1].Dir != "x/y" {
		t.Errorf("dirs = %q, %q", cmds[0].Dir, cmds[1].Dir)
	}
}

func TestValidateRaw(t *testing.T) {
	if errs := ValidateRaw("nope"); len(errs) != 1 {
		t.Errorf("non-list: %v", errs)
	}
	errs := ValidateRaw([]any{
		map[string]any{"name": "ok", "run": map[string]any{"__ref": map[string]any{}}},
		"x",
		map[string]any{"dir": "/abs"},
	})
	want := []string{"commands[1] must be an object", "commands[2].name is required", "commands[2].run is required", "commands[2].dir"}
	if len(errs) != len(want) {
		t.Fatalf("errs = %v", errs)
	}
	for i, w := range want {
		if !strings.HasPrefix(errs[i], w) {
			t.Errorf("errs[%d] = %q, want prefix %q", i, errs[i], w)
		}
	}
}
