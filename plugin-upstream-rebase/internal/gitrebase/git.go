package gitrebase

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
)

// Upstream refs are fetched under refs/upstream/, kept apart from the
// fork's own refs so "reachable from upstream" is unambiguous. Both tags
// and branches count: some forks rebase onto releases, others onto an
// untagged commit of upstream's main branch.
const (
	UpstreamNS      = "refs/upstream/"
	UpstreamTagsNS  = UpstreamNS + "tags/"
	UpstreamHeadsNS = UpstreamNS + "heads/"
)

// upstreamGlobs are the rev-list --glob arguments that select every
// upstream ref.
var upstreamGlobs = []string{"--glob=" + UpstreamTagsNS + "*", "--glob=" + UpstreamHeadsNS + "*"}

func git(r *runner.Runner, repo string, args ...string) (string, error) {
	out, err := r.Output(repo, "git", args...)
	return strings.TrimSpace(out), err
}

// RevParse resolves rev to a full SHA.
func RevParse(r *runner.Runner, repo, rev string) (string, error) {
	return git(r, repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// ── previous rebase point ─────────────────────────────────────────────────────

// Boundary is the previous rebase: Merge is the `git merge -s ours
// <old-fork-branch>` commit made on top of the old upstream tag, and
// UpstreamBase is that tag's commit (Merge^1).
type Boundary struct {
	Merge           string `json:"merge"`
	UpstreamBase    string `json:"upstream_base"`
	UpstreamBaseTag string `json:"upstream_base_tag,omitempty"`
}

// CheckBoundary verifies m has the shape of a previous rebase merge: a
// merge commit whose tree equals its first parent's (the signature of
// `-s ours`) and whose first parent is reachable from an upstream tag or
// branch.
// Returns "" when it does, else why not.
func CheckBoundary(r *runner.Runner, repo, m string) (string, error) {
	parents, err := git(r, repo, "rev-list", "--parents", "-n", "1", m)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(parents)
	if len(fields) < 3 {
		return "not a merge commit", nil
	}
	trees, err := git(r, repo, "rev-parse", m+"^{tree}", fields[1]+"^{tree}")
	if err != nil {
		return "", err
	}
	tt := strings.Fields(trees)
	if len(tt) != 2 || tt[0] != tt[1] {
		return "its tree differs from its first parent's (not a `merge -s ours` onto an upstream tag)", nil
	}
	contains, err := git(r, repo, "for-each-ref", "--count=1", "--contains", fields[1], "--format=%(refname)", UpstreamTagsNS, UpstreamHeadsNS)
	if err != nil {
		return "", err
	}
	if contains == "" {
		return "its first parent is not reachable from any upstream tag or branch", nil
	}
	return "", nil
}

// FindBoundary locates the previous rebase point on forkRef: walking
// the first-parent chain from the tip, the first merge that passes
// CheckBoundary. If override is non-empty it is verified instead of
// searched for.
func FindBoundary(r *runner.Runner, repo, forkRef, override string) (*Boundary, error) {
	var merge string
	if override != "" {
		sha, err := RevParse(r, repo, override)
		if err != nil || sha == "" {
			return nil, fmt.Errorf("previous_rebase_commit %q does not resolve to a commit", override)
		}
		if anc, _ := r.ExitCode(repo, "git", "merge-base", "--is-ancestor", sha, forkRef); anc != 0 {
			return nil, fmt.Errorf("previous_rebase_commit %s is not an ancestor of %s", sha, forkRef)
		}
		why, err := CheckBoundary(r, repo, sha)
		if err != nil {
			return nil, err
		}
		if why != "" {
			return nil, fmt.Errorf("previous_rebase_commit %s does not look like a rebase merge: %s", sha, why)
		}
		merge = sha
	} else {
		list, err := git(r, repo, "rev-list", "--first-parent", "--merges", forkRef)
		if err != nil {
			return nil, err
		}
		for _, m := range strings.Fields(list) {
			why, err := CheckBoundary(r, repo, m)
			if err != nil {
				return nil, err
			}
			if why == "" {
				merge = m
				break
			}
		}
		if merge == "" {
			return nil, fmt.Errorf("could not find the previous rebase on %s: no first-parent merge whose tree equals its first parent's and whose first parent is upstream content (reachable from an upstream tag or branch); set previous_rebase_commit explicitly", forkRef)
		}
	}
	base, err := RevParse(r, repo, merge+"^1")
	if err != nil {
		return nil, err
	}
	tag, _ := git(r, repo, "for-each-ref", "--points-at", base, "--format=%(refname:lstrip=3)", UpstreamTagsNS)
	if i := strings.IndexByte(tag, '\n'); i >= 0 {
		tag = tag[:i]
	}
	return &Boundary{Merge: merge, UpstreamBase: base, UpstreamBaseTag: tag}, nil
}

// ── enumeration ───────────────────────────────────────────────────────────────

// Commit is one commit that must be considered by the rebase.
type Commit struct {
	SHA     string `json:"orig_sha"`
	Author  string `json:"author"`
	Subject string `json:"subject"`
	Kind    Kind   `json:"kind"`
	PR      int    `json:"pr,omitempty"`
	PatchID string `json:"patch_id,omitempty"`
	// Detail is extra context for the UI (the combined diff of an evil
	// merge).
	Detail string `json:"detail,omitempty"`
}

// Enumerate returns, in pick order, every commit on forkRef since the
// previous rebase merge that isn't upstream content:
//
//	git log --reverse --topo-order --no-merges <forkRef> ^<merge>
//	        ^<target> --not --glob=refs/upstream/tags/*
//
// (and likewise refs/upstream/heads/*), followed by any evil merges in
// the same range (merges whose combined diff is non-empty), which have
// no pick order of their own. Excluding everything reachable from
// upstream tags and branches drops commits that arrived via mid-cycle
// upstream merges (patch releases, or newer upstream main).
func Enumerate(r *runner.Runner, repo, forkRef, merge, target string) ([]Commit, error) {
	rangeArgs := append([]string{forkRef, "^" + merge, "^" + target, "--not"}, upstreamGlobs...)
	const fs, rs = "\x1f", "\x1e"
	format := "--format=%H" + "%x1f" + "%an <%ae>" + "%x1f" + "%s" + "%x1f" + "%(trailers:key=" + PatchTrailer + ",valueonly,separator=%x2C)" + "%x1e"
	args := append([]string{"log", "--reverse", "--topo-order", "--no-merges", format}, rangeArgs...)
	out, err := r.Output(repo, "git", args...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range strings.Split(out, rs) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, fs)
		if len(f) < 4 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		c := Commit{SHA: f[0], Author: f[1], Subject: f[2], PatchID: strings.TrimSpace(f[3])}
		c.Kind, c.PR = Classify(c.Subject, c.PatchID)
		commits = append(commits, c)
	}

	margs := append([]string{"log", "--reverse", "--topo-order", "--merges", "--format=%H" + "%x1f" + "%an <%ae>" + "%x1f" + "%s" + "%x1e"}, rangeArgs...)
	mout, err := r.Output(repo, "git", margs...)
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(mout, rs) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, fs)
		if len(f) < 3 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		cc, err := r.Output(repo, "git", "diff-tree", "--cc", "--no-commit-id", "-p", f[0])
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(cc) == "" {
			continue
		}
		commits = append(commits, Commit{SHA: f[0], Author: f[1], Subject: f[2], Kind: KindEvilMerge, Detail: cc})
	}
	return commits, nil
}

// ── cherry-pick ───────────────────────────────────────────────────────────────

// Outcome is what happened when a commit was cherry-picked.
type Outcome string

const (
	OutcomePicked   Outcome = "picked"
	OutcomeEmpty    Outcome = "empty"
	OutcomeConflict Outcome = "conflict"
)

// CherryPick picks sha onto HEAD. On OutcomeEmpty and OutcomeConflict
// the cherry-pick is left in progress for the caller to resolve, skip,
// or continue.
func CherryPick(r *runner.Runner, repo, sha string) (Outcome, error) {
	err := r.Run(repo, "git", "cherry-pick", "--allow-empty", "--empty=stop", sha)
	if err == nil {
		return OutcomePicked, nil
	}
	if !InProgress(r, repo) {
		return "", fmt.Errorf("git cherry-pick %s: %w", sha, err)
	}
	unmerged, uerr := git(r, repo, "ls-files", "-u")
	if uerr != nil {
		return "", uerr
	}
	if unmerged != "" {
		return OutcomeConflict, nil
	}
	staged, serr := r.HasStagedChanges(repo)
	if serr != nil {
		return "", serr
	}
	if !staged {
		return OutcomeEmpty, nil
	}
	return "", fmt.Errorf("git cherry-pick %s stopped without conflicts or emptiness: %w", sha, err)
}

// InProgress reports whether a cherry-pick is in progress.
func InProgress(r *runner.Runner, repo string) bool {
	code, err := r.ExitCode(repo, "git", "rev-parse", "--verify", "--quiet", "CHERRY_PICK_HEAD")
	return err == nil && code == 0
}

// Skip abandons the in-progress pick.
func Skip(r *runner.Runner, repo string) error {
	return r.Run(repo, "git", "cherry-pick", "--skip")
}

// Continue commits the in-progress pick with the original message. If
// the resolution left nothing staged, returns committed=false after
// skipping the pick instead.
func Continue(r *runner.Runner, repo string) (committed bool, err error) {
	staged, err := r.HasStagedChanges(repo)
	if err != nil {
		return false, err
	}
	if !staged {
		return false, Skip(r, repo)
	}
	if err := r.Run(repo, "git", "-c", "core.editor=true", "cherry-pick", "--continue"); err != nil {
		return false, err
	}
	return true, nil
}

// KeepEmpty commits the in-progress (empty) pick as an empty commit
// with the original message.
func KeepEmpty(r *runner.Runner, repo, sha string) error {
	if err := r.Run(repo, "git", "commit", "--allow-empty", "--no-edit", "-C", sha); err != nil {
		return err
	}
	// The commit consumed the pick; clear the sequencer state.
	if InProgress(r, repo) {
		_, _ = r.ExitCode(repo, "git", "cherry-pick", "--quit")
	}
	return nil
}

// ── conflicts ─────────────────────────────────────────────────────────────────

// ConflictFile is one unmerged path, with every side's content for the
// resolution UI. Content fields hold text; when Binary is true they hold
// base64 and the UI only offers take-ours / take-theirs / delete.
type ConflictFile struct {
	Path   string `json:"path"`
	Binary bool   `json:"binary,omitempty"`
	// Base (stage 1), Ours (stage 2 - the new upstream side), Theirs
	// (stage 3 - the fork commit being picked). Nil when that side
	// doesn't have the file (add/add, modify/delete).
	Base   *string `json:"base"`
	Ours   *string `json:"ours"`
	Theirs *string `json:"theirs"`
	// Merged is the worktree file with conflict markers.
	Merged *string `json:"merged"`
	// Suggestion is what git rerere resolved this path to, when it
	// recognised the conflict ("suggest" mode).
	Suggestion *string `json:"suggestion,omitempty"`
}

type stageEntry struct {
	mode, blob string
	stage      int
	path       string
}

func unmergedEntries(r *runner.Runner, repo string) ([]stageEntry, error) {
	out, err := r.Output(repo, "git", "ls-files", "-u", "-z")
	if err != nil {
		return nil, err
	}
	var entries []stageEntry
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("unexpected ls-files record %q", rec)
		}
		meta := strings.Fields(rec[:tab])
		if len(meta) != 3 {
			return nil, fmt.Errorf("unexpected ls-files record %q", rec)
		}
		stage := int(meta[2][0] - '0')
		entries = append(entries, stageEntry{mode: meta[0], blob: meta[1], stage: stage, path: rec[tab+1:]})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].path != entries[j].path {
			return entries[i].path < entries[j].path
		}
		return entries[i].stage < entries[j].stage
	})
	return entries, nil
}

// Fingerprint identifies a conflict exactly: sha256 over the sorted
// unmerged index entries (path, stage, mode, blob). Identical base /
// ours / theirs for every conflicted path → identical fingerprint →
// a previously saved resolution is valid verbatim.
func Fingerprint(r *runner.Runner, repo string) (string, error) {
	entries, err := unmergedEntries(r, repo)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", errors.New("no unmerged paths")
	}
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\n", e.path, e.stage, e.mode, e.blob)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ConflictPaths lists the unmerged paths.
func ConflictPaths(r *runner.Runner, repo string) ([]string, error) {
	entries, err := unmergedEntries(r, repo)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if len(paths) == 0 || paths[len(paths)-1] != e.path {
			paths = append(paths, e.path)
		}
	}
	return paths, nil
}

// CaptureConflict collects every side of every unmerged path. When
// rerere already resolved a path in the worktree (rerere enabled, no
// autoupdate), that content becomes the Suggestion and the conflict
// markers are regenerated for Merged.
func CaptureConflict(r *runner.Runner, repo string, rerereOn bool) ([]ConflictFile, error) {
	entries, err := unmergedEntries(r, repo)
	if err != nil {
		return nil, err
	}
	remaining := map[string]bool{}
	if rerereOn {
		out, err := r.Output(repo, "git", "rerere", "remaining")
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(out, "\n") {
			if p != "" {
				remaining[p] = true
			}
		}
	}
	byPath := map[string]*ConflictFile{}
	var order []string
	blobs := map[string][]byte{}
	for _, e := range entries {
		f, ok := byPath[e.path]
		if !ok {
			f = &ConflictFile{Path: e.path}
			byPath[e.path] = f
			order = append(order, e.path)
		}
		data, err := r.Output(repo, "git", "cat-file", "blob", e.blob)
		if err != nil {
			return nil, err
		}
		blobs[e.path+"\x00"+fmt.Sprint(e.stage)] = []byte(data)
		if isBinary([]byte(data)) {
			f.Binary = true
		}
	}
	for _, p := range order {
		f := byPath[p]
		enc := func(b []byte, ok bool) *string {
			if !ok {
				return nil
			}
			var s string
			if f.Binary {
				s = base64.StdEncoding.EncodeToString(b)
			} else {
				s = string(b)
			}
			return &s
		}
		b1, ok1 := blobs[p+"\x001"]
		b2, ok2 := blobs[p+"\x002"]
		b3, ok3 := blobs[p+"\x003"]
		f.Base, f.Ours, f.Theirs = enc(b1, ok1), enc(b2, ok2), enc(b3, ok3)

		full := filepath.Join(repo, p)
		wt, wtErr := os.ReadFile(full)
		if rerereOn && !remaining[p] && wtErr == nil && !f.Binary {
			s := string(wt)
			f.Suggestion = &s
			// Regenerate markers so Merged shows the real conflict.
			if ok2 && ok3 {
				_, _ = r.ExitCode(repo, "git", "checkout", "--conflict=merge", "--", p)
				wt, wtErr = os.ReadFile(full)
			}
		}
		if wtErr == nil {
			f.Merged = enc(wt, true)
		}
	}
	files := make([]ConflictFile, 0, len(order))
	for _, p := range order {
		files = append(files, *byPath[p])
	}
	return files, nil
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return !utf8.Valid(b)
}

// FileResolution is the resolved state of one path. Exactly one of
// Content / Deleted / Take is meaningful.
type FileResolution struct {
	// Content is the final file content (text).
	Content *string `json:"content,omitempty"`
	// Deleted removes the path.
	Deleted bool `json:"deleted,omitempty"`
	// Take is "ours" (new upstream side) or "theirs" (the fork commit);
	// works for binary files too.
	Take string `json:"take,omitempty"`
}

// ApplyResolution writes files into the worktree and stages them. Paths
// that aren't conflicted are allowed (a resolution may also adjust
// other files the pick touched). Returns an error if any path is still
// unmerged afterwards.
func ApplyResolution(r *runner.Runner, repo string, files map[string]FileResolution) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fr := files[p]
		if strings.HasPrefix(p, "/") || strings.Contains("/"+filepath.ToSlash(filepath.Clean(p))+"/", "/../") {
			return fmt.Errorf("resolution path %q escapes the repo", p)
		}
		full := filepath.Join(repo, p)
		switch {
		case fr.Deleted:
			if err := r.Run(repo, "git", "rm", "--quiet", "--force", "--ignore-unmatch", "--", p); err != nil {
				return err
			}
		case fr.Take == "ours" || fr.Take == "theirs":
			if err := r.Run(repo, "git", "checkout", "--"+fr.Take, "--", p); err != nil {
				// The chosen side doesn't have the file (modify/delete):
				// taking that side means deleting it.
				if rmErr := r.Run(repo, "git", "rm", "--quiet", "--force", "--ignore-unmatch", "--", p); rmErr != nil {
					return fmt.Errorf("take %s for %s: %w", fr.Take, p, err)
				}
				continue
			}
			if err := r.Run(repo, "git", "add", "--", p); err != nil {
				return err
			}
		case fr.Content != nil:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(*fr.Content), 0o644); err != nil {
				return err
			}
			if err := r.Run(repo, "git", "add", "--", p); err != nil {
				return err
			}
		default:
			return fmt.Errorf("resolution for %s has no content, deleted, or take", p)
		}
	}
	left, err := ConflictPaths(r, repo)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("resolution leaves paths unmerged: %s", strings.Join(left, ", "))
	}
	return nil
}

// StageRerereResolved stages every conflicted path rerere has fully
// resolved (rerere "auto" mode). Returns true when nothing is left
// unmerged.
func StageRerereResolved(r *runner.Runner, repo string) (bool, error) {
	remainingOut, err := r.Output(repo, "git", "rerere", "remaining")
	if err != nil {
		return false, err
	}
	remaining := map[string]bool{}
	for _, p := range strings.Split(remainingOut, "\n") {
		if p != "" {
			remaining[p] = true
		}
	}
	paths, err := ConflictPaths(r, repo)
	if err != nil {
		return false, err
	}
	for _, p := range paths {
		if remaining[p] {
			continue
		}
		if err := r.Run(repo, "git", "add", "--", p); err != nil {
			return false, err
		}
	}
	left, err := ConflictPaths(r, repo)
	if err != nil {
		return false, err
	}
	return len(left) == 0, nil
}

// ── rerere cache ──────────────────────────────────────────────────────────────

// RerereEntry is one .git/rr-cache/<id> directory: its files
// (preimage, postimage, preimage.1, …) by name.
type RerereEntry struct {
	ID    string            `json:"id"`
	Files map[string]string `json:"files"`
}

func rrCacheDir(r *runner.Runner, repo string) (string, error) {
	gd, err := git(r, repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(repo, gd)
	}
	return filepath.Join(gd, "rr-cache"), nil
}

// ImportRerere writes entries into the repo's rr-cache.
func ImportRerere(r *runner.Runner, repo string, entries []RerereEntry) error {
	dir, err := rrCacheDir(r, repo)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID == "" || strings.ContainsAny(e.ID, "/\\.") {
			continue
		}
		ed := filepath.Join(dir, e.ID)
		if err := os.MkdirAll(ed, 0o755); err != nil {
			return err
		}
		for name, content := range e.Files {
			if strings.ContainsAny(name, "/\\") {
				continue
			}
			if err := os.WriteFile(filepath.Join(ed, name), []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExportRerere reads every rr-cache entry that has at least one
// recorded resolution (a postimage).
func ExportRerere(r *runner.Runner, repo string) ([]RerereEntry, error) {
	dir, err := rrCacheDir(r, repo)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RerereEntry
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, d.Name()))
		if err != nil {
			return nil, err
		}
		e := RerereEntry{ID: d.Name(), Files: map[string]string{}}
		hasPost := false
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, d.Name(), f.Name()))
			if err != nil {
				return nil, err
			}
			e.Files[f.Name()] = string(b)
			if strings.HasPrefix(f.Name(), "postimage") {
				hasPost = true
			}
		}
		if hasPost {
			out = append(out, e)
		}
	}
	return out, nil
}

// RerereIDs maps each path in the in-progress conflict to the rr-cache
// entry (directory name) rerere is tracking it under, read from
// .git/MERGE_RR. Empty when rerere is disabled or nothing is tracked.
// Used to link stored rerere entries to the resolution / commit that
// produced them, so they can be deleted together.
func RerereIDs(r *runner.Runner, repo string) (map[string]string, error) {
	gd, err := git(r, repo, "rev-parse", "--git-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(repo, gd)
	}
	b, err := os.ReadFile(filepath.Join(gd, "MERGE_RR"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	// Records are "<hash>[.<variant>]\t<path>\0".
	for _, rec := range strings.Split(string(b), "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab <= 0 {
			continue
		}
		id := rec[:tab]
		if dot := strings.IndexByte(id, '.'); dot >= 0 {
			id = id[:dot]
		}
		out[rec[tab+1:]] = id
	}
	return out, nil
}

// PruneRerere deletes every *completed* rr-cache entry (one with a
// recorded postimage) not in keep. A resumed working copy calls this so
// entries deleted from the plugin's store while the run was suspended
// don't come back on the next export. Entries without a postimage are
// always kept: they're the preimages of the conflict the run suspended
// on, never stored yet, and rerere needs them to record the resolution
// when the resumed pick commits.
func PruneRerere(r *runner.Runner, repo string, keep map[string]bool) error {
	dir, err := rrCacheDir(r, repo)
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, d := range ents {
		if !d.IsDir() || keep[d.Name()] {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, d.Name()))
		if err != nil {
			return err
		}
		completed := false
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "postimage") {
				completed = true
				break
			}
		}
		if !completed {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, d.Name())); err != nil {
			return err
		}
	}
	return nil
}

// PatchIDs returns `git patch-id --stable` for each commit: a hash of
// the commit's diff ignoring whitespace and line numbers. Two commits
// with equal patch-ids carry the same change. Computed in one batched
// `git show | git patch-id` pass. Commits with an empty diff are
// absent from the result.
func PatchIDs(r *runner.Runner, repo string, shas []string) (map[string]string, error) {
	out := map[string]string{}
	if len(shas) == 0 {
		return out, nil
	}
	show := exec.CommandContext(r.Ctx, "git", append([]string{"show", "--no-color", "--pretty=format:commit %H", "-p"}, shas...)...)
	show.Dir, show.Env = repo, r.Env
	pid := exec.CommandContext(r.Ctx, "git", "patch-id", "--stable")
	pid.Dir, pid.Env = repo, r.Env
	pipe, err := show.StdoutPipe()
	if err != nil {
		return nil, err
	}
	pid.Stdin = pipe
	var stdout, stderr strings.Builder
	pid.Stdout, pid.Stderr = &stdout, &stderr
	show.Stderr = &stderr
	// Recorded with patch-id's output only: the `git show` side is the
	// full diff of every commit, already recoverable from the repo.
	done := r.Record(runner.KindQuery, repo,
		"git show --no-color --pretty=format:commit %H -p "+strings.Join(shas, " ")+" | git patch-id --stable")
	if err := pid.Start(); err != nil {
		done(-1, nil, err)
		return nil, err
	}
	if err := show.Run(); err != nil {
		_ = pid.Wait()
		done(runner.ExitCodeOf(err), []byte(stderr.String()), err)
		return nil, fmt.Errorf("git show for patch-id: %w\n%s", err, stderr.String())
	}
	if err := pid.Wait(); err != nil {
		done(runner.ExitCodeOf(err), []byte(stdout.String()+stderr.String()), err)
		return nil, fmt.Errorf("git patch-id: %w\n%s", err, stderr.String())
	}
	done(0, []byte(stdout.String()+stderr.String()), nil)
	// Lines are "<patch-id> <commit>".
	for _, line := range strings.Split(stdout.String(), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[f[1]] = f[0]
		}
	}
	return out, nil
}
