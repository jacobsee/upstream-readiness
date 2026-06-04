// Package godeps is the per-module-directory dependency pipeline the
// repo's Go plugins share:
//
//	go get / replace → go mod tidy → deps commit
//	  → go mod vendor → vendor commit
//	  → AfterVendor hook (the per-step patch queue)
//	  → one commit per make target
//
// generic-go-dep runs it as its whole job; upstream-rebase runs it after
// replaying the fork's UPSTREAM commits. Commit messages are injected so
// each plugin labels its commits its own way while the mechanics stay
// identical.
package godeps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jacobsee/upstream-readiness/pluginkit/gitenv"
	"github.com/jacobsee/upstream-readiness/pluginkit/refs"
	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// Dir is one Go module directory inside the cloned repo. Every command
// the pipeline runs for this entry (go get, go mod tidy, go mod vendor,
// make) uses this dir as its working directory. `git` commands are also
// run with this cwd, which changes nothing about the index they touch
// (git walks up to the worktree root) but means `git add go.mod go.sum`
// stages the files in *this* dir's module.
type Dir struct {
	// Path is the module dir relative to the repo root. Use "." for
	// the root module. Absolute paths and ".." segments are rejected.
	Path string `json:"path"`
	// Updates is the list of module bumps applied to this dir's go.mod.
	// Each entry is independently subject to auto-replace resolution.
	Updates []Update `json:"updates"`
	// MakeTargets is the list of make targets to run in this dir after
	// the deps commit. Falls back to the instance's default_make_targets
	// when empty. Each target's file changes are committed separately.
	MakeTargets []string `json:"make_targets"`
	// RunVendor toggles `go mod vendor` after the dep bump. The vendored
	// tree lands in its own commit so reviewers can skim the actual
	// dep change without wading through mechanical vendor churn.
	RunVendor bool `json:"run_vendor"`
}

// Update is a single module bump. TargetVersion may be a literal
// semver / SHA / branch name, or a {"__ref": {...}} marker (resolved
// before this struct is decoded) that names one of an upstream step's
// outputs.
type Update struct {
	Dependency    string `json:"dependency"`
	TargetVersion string `json:"target_version"`
}

// AppliedBump records what actually happened for one Update.
type AppliedBump struct {
	// Path is the module dir the bump was applied in ("." for root).
	Path          string `json:"path"`
	Dependency    string `json:"dependency"`
	TargetVersion string `json:"target_version"`
	// Replace, if non-empty, describes the fork-based replace directive
	// the plugin inserted for this bump. Empty when the bump resolved
	// through the canonical module path.
	Replace *ReplaceRecord `json:"replace,omitempty"`
}

// ReplaceRecord describes an inserted fork replace directive.
type ReplaceRecord struct {
	ForkModulePath string `json:"fork_module_path"`
	ForkCommit     string `json:"fork_commit"`
	UpstreamStepID string `json:"upstream_step_id"`
}

// Options tweak validation for plugins whose dirs don't always bump.
type Options struct {
	// AllowNoUpdates lets a dir carry zero updates (e.g. a rebase that
	// only needs `make update`). Such dirs skip the bump/tidy/deps
	// commit stage and go straight to vendor / patches / make.
	AllowNoUpdates bool
}

// ValidateRaw checks a still-unresolved params `dirs` value (as decoded
// into `any`) for shape problems. Step params may contain unresolved
// __ref markers at edit time, so only key presence is checked, not
// value types. Returns human-readable errors.
func ValidateRaw(dirsRaw any, present bool, opts Options) []string {
	var errs []string
	if !present {
		return append(errs, "dirs is required (list of {path, updates, make_targets?, run_vendor?} entries)")
	}
	dirs, ok := dirsRaw.([]any)
	if !ok {
		return append(errs, "dirs must be a list")
	}
	if len(dirs) == 0 {
		return append(errs, "dirs must contain at least one entry")
	}
	for i, d := range dirs {
		m, ok := d.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("dirs[%d] must be an object", i))
			continue
		}
		if p, ok := m["path"]; !ok {
			errs = append(errs, fmt.Sprintf("dirs[%d].path is required (use \".\" for the repo root)", i))
		} else if ps, ok := p.(string); !ok {
			errs = append(errs, fmt.Sprintf("dirs[%d].path must be a string", i))
		} else if perr := gitenv.CheckModuleDir(ps); perr != "" {
			errs = append(errs, fmt.Sprintf("dirs[%d].path %s", i, perr))
		}
		updatesRaw, ok := m["updates"]
		if !ok {
			if opts.AllowNoUpdates {
				continue
			}
			errs = append(errs, fmt.Sprintf("dirs[%d].updates is required (list of {dependency, target_version} entries)", i))
			continue
		}
		updates, ok := updatesRaw.([]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("dirs[%d].updates must be a list", i))
			continue
		}
		if len(updates) == 0 {
			if !opts.AllowNoUpdates {
				errs = append(errs, fmt.Sprintf("dirs[%d].updates must contain at least one entry", i))
			}
			continue
		}
		for j, u := range updates {
			um, ok := u.(map[string]any)
			if !ok {
				errs = append(errs, fmt.Sprintf("dirs[%d].updates[%d] must be an object", i, j))
				continue
			}
			if _, ok := um["dependency"]; !ok {
				errs = append(errs, fmt.Sprintf("dirs[%d].updates[%d].dependency is required", i, j))
			}
			if _, ok := um["target_version"]; !ok {
				errs = append(errs, fmt.Sprintf("dirs[%d].updates[%d].target_version is required (literal or __ref)", i, j))
			}
		}
	}
	return errs
}

// Normalize validates resolved dirs at Execute time, cleans each path,
// and fills in the instance's default make targets where a dir has
// none. Mutates dirs in place.
func Normalize(dirs []Dir, defaultMakeTargets []string, opts Options) error {
	if len(dirs) == 0 {
		return fmt.Errorf("dirs must contain at least one entry")
	}
	seenDir := map[string]int{}
	for i := range dirs {
		d := &dirs[i]
		if d.Path == "" {
			return fmt.Errorf("dirs[%d].path is required (use \".\" for the repo root)", i)
		}
		if perr := gitenv.CheckModuleDir(d.Path); perr != "" {
			return fmt.Errorf("dirs[%d].path %s", i, perr)
		}
		d.Path = filepath.Clean(d.Path)
		if prev, dup := seenDir[d.Path]; dup {
			return fmt.Errorf("dirs[%d].path %q duplicates dirs[%d]", i, d.Path, prev)
		}
		seenDir[d.Path] = i
		if len(d.Updates) == 0 && !opts.AllowNoUpdates {
			return fmt.Errorf("dirs[%d] (%s): updates must contain at least one entry", i, d.Path)
		}
		seenDep := map[string]bool{}
		for j, u := range d.Updates {
			if u.Dependency == "" {
				return fmt.Errorf("dirs[%d].updates[%d].dependency is required", i, j)
			}
			if u.TargetVersion == "" {
				return fmt.Errorf("dirs[%d].updates[%d].target_version is required", i, j)
			}
			if seenDep[u.Dependency] {
				return fmt.Errorf("dirs[%d] (%s): updates lists dependency %q more than once", i, d.Path, u.Dependency)
			}
			seenDep[u.Dependency] = true
		}
		if len(d.MakeTargets) == 0 {
			d.MakeTargets = defaultMakeTargets
		}
	}
	return nil
}

// DetectAutoReplaces works out, per (dir, update), whether a fork
// replace directive is needed. rawParams is the *pre-resolution* params
// object: rawParams["dirs"][i]["updates"][j] still holds the __ref
// marker that tells us which upstream step a target_version came from.
func DetectAutoReplaces(rawParams map[string]any, dirs []Dir, upstream map[string]*pluginpb.StepOutputs) ([][]*refs.AutoReplaceDirective, error) {
	rawDirs, _ := rawParams["dirs"].([]any)
	out := make([][]*refs.AutoReplaceDirective, len(dirs))
	for i, d := range dirs {
		ars := make([]*refs.AutoReplaceDirective, len(d.Updates))
		var rawDirUpdates []any
		if i < len(rawDirs) {
			if rd, ok := rawDirs[i].(map[string]any); ok {
				rawDirUpdates, _ = rd["updates"].([]any)
			}
		}
		for j, u := range d.Updates {
			var rawUpdate map[string]any
			if j < len(rawDirUpdates) {
				rawUpdate, _ = rawDirUpdates[j].(map[string]any)
			}
			ar, err := refs.DetectAutoReplace(rawUpdate, upstream, u.Dependency)
			if err != nil {
				return nil, fmt.Errorf("dirs[%d].updates[%d]: %w", i, j, err)
			}
			ars[j] = ar
		}
		out[i] = ars
	}
	return out, nil
}

// ForkHosts returns the git hosts the auto-replaces will fetch from, so
// gitenv.BuildGitEnv can install SSH insteadOf rewrites for them.
func ForkHosts(dirAutoReplaces [][]*refs.AutoReplaceDirective) []string {
	var hosts []string
	for _, ars := range dirAutoReplaces {
		for _, ar := range ars {
			if ar != nil {
				hosts = append(hosts, gitenv.GitHost(ar.ForkURL))
			}
		}
	}
	return hosts
}

// Messages formats the commits ProcessDir creates.
type Messages struct {
	Deps   func(path string, bumps []AppliedBump) string
	Vendor func(path string) string
	Make   func(path, target string) string
}

// Hooks customise ProcessDir per plugin.
type Hooks struct {
	Messages Messages
	// AfterVendor runs after `go mod vendor` and before the make
	// targets - where the per-step patch queue is applied, so make
	// targets exercise the patched code. May be nil.
	AfterVendor func(d Dir, dirCwd string) error
}

// ProcessDir runs the pipeline for one dir and returns the applied bump
// records. autoReplaces is parallel to d.Updates.
func ProcessDir(r *runner.Runner, repoDir string, d Dir, autoReplaces []*refs.AutoReplaceDirective, h Hooks) ([]AppliedBump, error) {
	dirCwd := filepath.Join(repoDir, d.Path)
	pathLabel := d.Path
	if pathLabel == "." {
		pathLabel = "(root)"
	}

	var dirBumps []AppliedBump
	if len(d.Updates) > 0 {
		r.Emit("bumping", fmt.Sprintf("[%s] applying dependency updates", pathLabel), r.Tail())
		// Apply every replace directive first so subsequent `go get` /
		// `go mod tidy` calls resolve through them. Updates without an
		// auto-replace get plain `go get dep@version` treatment.
		//
		// `go mod edit -replace` refuses raw commit SHAs, so each fork
		// SHA is resolved to its pseudo-version first. This also means
		// later iterations won't trip over an earlier iteration's
		// replace when go mod edit re-parses go.mod.
		for ui, u := range d.Updates {
			ar := autoReplaces[ui]
			if ar != nil {
				pseudo, err := r.ResolvePseudo(dirCwd, ar.ForkModulePath, ar.ForkCommit)
				if err != nil {
					return nil, fmt.Errorf("[%s] resolve pseudo-version for %s@%s: %w", d.Path, ar.ForkModulePath, ar.ForkCommit, err)
				}
				// Record what actually went into go.mod so downstream
				// consumers (bump record, commit message) reflect the
				// resolved version, not the raw SHA that go rejects.
				ar.ForkCommit = pseudo
				replaceArg := fmt.Sprintf("%s=%s@%s", ar.Dependency, ar.ForkModulePath, pseudo)
				if err := r.Run(dirCwd, "go", "mod", "edit", "-replace", replaceArg); err != nil {
					return nil, fmt.Errorf("[%s] go mod edit -replace %s: %w", d.Path, replaceArg, err)
				}
			} else {
				if err := r.Run(dirCwd, "go", "get", u.Dependency+"@"+u.TargetVersion); err != nil {
					return nil, fmt.Errorf("[%s] go get %s@%s: %w", d.Path, u.Dependency, u.TargetVersion, err)
				}
			}
		}
		r.Emit("tidying", fmt.Sprintf("[%s] go mod tidy", pathLabel), r.Tail())
		if err := r.Run(dirCwd, "go", "mod", "tidy"); err != nil {
			return nil, fmt.Errorf("[%s] go mod tidy: %w", d.Path, err)
		}

		// Deps commit - go.mod/go.sum captures both the direct bumps and
		// any transitive updates go mod tidy produced. Committed before
		// vendor/make so reviewers can read the actual dependency change
		// on its own.
		if err := r.Run(dirCwd, "git", "add", "go.mod", "go.sum"); err != nil {
			return nil, fmt.Errorf("[%s] git add: %w", d.Path, err)
		}

		dirBumps = make([]AppliedBump, len(d.Updates))
		for ui, u := range d.Updates {
			b := AppliedBump{Path: d.Path, Dependency: u.Dependency, TargetVersion: u.TargetVersion}
			if ar := autoReplaces[ui]; ar != nil {
				b.Replace = &ReplaceRecord{
					ForkModulePath: ar.ForkModulePath,
					ForkCommit:     ar.ForkCommit,
					UpstreamStepID: ar.UpstreamStepID,
				}
			}
			dirBumps[ui] = b
		}
		if err := r.Run(dirCwd, "git", "commit", "-m", h.Messages.Deps(d.Path, dirBumps)); err != nil {
			return nil, fmt.Errorf("[%s] git commit: %w", d.Path, err)
		}
	}

	if d.RunVendor {
		r.Emit("vendoring", fmt.Sprintf("[%s] go mod vendor", pathLabel), r.Tail())
		if err := r.Run(dirCwd, "go", "mod", "vendor"); err != nil {
			return nil, fmt.Errorf("[%s] go mod vendor: %w", d.Path, err)
		}
		if err := r.Run(dirCwd, "git", "add", "-A"); err != nil {
			return nil, fmt.Errorf("[%s] git add after go mod vendor: %w", d.Path, err)
		}
		hasChanges, err := r.HasStagedChanges(dirCwd)
		if err != nil {
			return nil, fmt.Errorf("[%s] check staged changes after go mod vendor: %w", d.Path, err)
		}
		if hasChanges {
			if err := r.Run(dirCwd, "git", "commit", "-m", h.Messages.Vendor(d.Path)); err != nil {
				return nil, fmt.Errorf("[%s] git commit vendor: %w", d.Path, err)
			}
		} else {
			r.Note(fmt.Sprintf("# [%s] go mod vendor produced no changes - no commit\n", d.Path))
		}
	}

	if h.AfterVendor != nil {
		if err := h.AfterVendor(d, dirCwd); err != nil {
			return nil, err
		}
	}

	// One commit per make target so reviewers can attribute file
	// changes to the specific target that produced them. Targets that
	// leave the tree unmodified are silently skipped.
	for _, target := range d.MakeTargets {
		r.Emit("testing", fmt.Sprintf("[%s] make %s", pathLabel, target), r.Tail())
		if err := r.Run(dirCwd, "make", target); err != nil {
			return nil, fmt.Errorf("[%s] make %s: %w", d.Path, target, err)
		}
		if err := r.Run(dirCwd, "git", "add", "-A"); err != nil {
			return nil, fmt.Errorf("[%s] git add after make %s: %w", d.Path, target, err)
		}
		hasChanges, err := r.HasStagedChanges(dirCwd)
		if err != nil {
			return nil, fmt.Errorf("[%s] check staged changes after make %s: %w", d.Path, target, err)
		}
		if !hasChanges {
			r.Note(fmt.Sprintf("# [%s] make %s produced no changes - no commit\n", d.Path, target))
			continue
		}
		if err := r.Run(dirCwd, "git", "commit", "-m", h.Messages.Make(d.Path, target)); err != nil {
			return nil, fmt.Errorf("[%s] git commit after make %s: %w", d.Path, target, err)
		}
	}
	return dirBumps, nil
}

// DeriveBranchName produces a stable branch name from all bumps across
// every dir when the workflow author didn't set one explicitly. A
// single-update / single-dir step reproduces the original
// `update/<dep>/<version>` shape; anything larger hashes the sorted
// (path, dep, version) tuples so the name is deterministic across runs.
func DeriveBranchName(dirs []Dir) string {
	total := 0
	for _, d := range dirs {
		total += len(d.Updates)
	}
	if total == 1 {
		for _, d := range dirs {
			if len(d.Updates) == 1 {
				u := d.Updates[0]
				return fmt.Sprintf("update/%s/%s", gitenv.Slugify(u.Dependency), gitenv.Slugify(u.TargetVersion))
			}
		}
	}
	keys := make([]string, 0, total)
	for _, d := range dirs {
		for _, u := range d.Updates {
			keys = append(keys, d.Path+":"+u.Dependency+"@"+u.TargetVersion)
		}
	}
	sort.Strings(keys)
	h := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return fmt.Sprintf("update/multi/%d-deps-%s", total, hex.EncodeToString(h[:])[:12])
}

// BumpLines renders the bullet list used by multi-bump commit messages,
// including the per-bump temporary-replace note.
func BumpLines(bumps []AppliedBump) string {
	var b strings.Builder
	for _, bump := range bumps {
		fmt.Fprintf(&b, "- %s -> %s", bump.Dependency, bump.TargetVersion)
		if bump.Replace != nil {
			fmt.Fprintf(&b, "\n    (temporary replace: => %s %s, from step %q)",
				bump.Replace.ForkModulePath, bump.Replace.ForkCommit, bump.Replace.UpstreamStepID)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// AnyHasReplace reports whether any bump inserted a fork replace.
func AnyHasReplace(bumps []AppliedBump) bool {
	for _, b := range bumps {
		if b.Replace != nil {
			return true
		}
	}
	return false
}
