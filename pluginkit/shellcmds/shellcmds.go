// Package shellcmds runs a step's bare shell commands, the last thing
// the git plugins do before pushing. Each command goes through `sh -c`
// in its dir (relative to the repo root) on the recorded runner, and
// whatever it changes lands in its own commit. A command that leaves
// the tree unmodified gets no commit; a non-zero exit fails the step.
// Commit messages are injected so each plugin labels its commits its
// own way while the mechanics stay identical.
package shellcmds

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jacobsee/upstream-readiness/pluginkit/gitenv"
	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
)

// Command is one entry of a step's `commands` param.
type Command struct {
	// Name is the commit subject, after the plugin's own prefix.
	Name string `json:"name"`
	// Run is handed to `sh -c`.
	Run string `json:"run"`
	// Dir is the working directory relative to the repo root. Default ".".
	Dir string `json:"dir"`
}

// ValidateRaw checks a still-unresolved params `commands` value (as
// decoded into `any`). Step params may contain unresolved __ref markers
// at edit time, so only key presence is checked, not value types.
func ValidateRaw(cmdsRaw any) []string {
	cmds, ok := cmdsRaw.([]any)
	if !ok {
		return []string{"commands must be a list of {name, run, dir?} entries"}
	}
	var errs []string
	for i, c := range cmds {
		m, ok := c.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("commands[%d] must be an object", i))
			continue
		}
		for _, k := range []string{"name", "run"} {
			if _, ok := m[k]; !ok {
				errs = append(errs, fmt.Sprintf("commands[%d].%s is required", i, k))
			}
		}
		if d, ok := m["dir"].(string); ok {
			if perr := gitenv.CheckModuleDir(d); perr != "" {
				errs = append(errs, fmt.Sprintf("commands[%d].dir %s", i, perr))
			}
		}
	}
	return errs
}

// Normalize validates resolved commands at Execute time and cleans each
// dir. Mutates cmds in place.
func Normalize(cmds []Command) error {
	for i := range cmds {
		c := &cmds[i]
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("commands[%d].name is required (it becomes the commit subject)", i)
		}
		if strings.ContainsAny(c.Name, "\r\n") {
			return fmt.Errorf("commands[%d].name must be a single line", i)
		}
		if strings.TrimSpace(c.Run) == "" {
			return fmt.Errorf("commands[%d].run is required", i)
		}
		if c.Dir == "" {
			c.Dir = "."
		}
		if perr := gitenv.CheckModuleDir(c.Dir); perr != "" {
			return fmt.Errorf("commands[%d].dir %s", i, perr)
		}
		c.Dir = filepath.Clean(c.Dir)
	}
	return nil
}

// Run runs cmds in order in repoDir, committing each one's changes with
// message(c). cmds must have been through Normalize.
func Run(r *runner.Runner, repoDir string, cmds []Command, message func(Command) string) error {
	for i, c := range cmds {
		label := c.Name
		if c.Dir != "." {
			label = "(" + c.Dir + ") " + label
		}
		r.Emit("commands", fmt.Sprintf("[%d/%d] %s", i+1, len(cmds), label), r.Tail())
		if err := r.Run(filepath.Join(repoDir, c.Dir), "sh", "-c", c.Run); err != nil {
			return fmt.Errorf("commands[%d] %q: %w", i, c.Name, err)
		}
		if err := r.Run(repoDir, "git", "add", "-A"); err != nil {
			return fmt.Errorf("commands[%d] %q: git add: %w", i, c.Name, err)
		}
		hasChanges, err := r.HasStagedChanges(repoDir)
		if err != nil {
			return fmt.Errorf("commands[%d] %q: check staged changes: %w", i, c.Name, err)
		}
		if !hasChanges {
			r.Note(fmt.Sprintf("# command %q produced no changes - no commit\n", c.Name))
			continue
		}
		if err := r.Run(repoDir, "git", "commit", "-m", message(c)); err != nil {
			return fmt.Errorf("commands[%d] %q: git commit: %w", i, c.Name, err)
		}
	}
	return nil
}
