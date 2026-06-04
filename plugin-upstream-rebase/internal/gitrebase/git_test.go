package gitrebase

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		subject, patchID string
		kind             Kind
		pr               int
	}{
		{"UPSTREAM: <carry>: openshift auth hook", "", KindCarry, 0},
		{"UPSTREAM: carry: unbracketed", "", KindCarry, 0},
		{"UPSTREAM: <drop>: make update", "", KindDrop, 0},
		{"UPSTREAM: drop: regenerate", "", KindDrop, 0},
		{"UPSTREAM: 12345: fix race", "", KindPR, 12345},
		{"UPSTREAM: <12345>: bracketed pr", "", KindPR, 12345},
		{"UPSTREAM <carry>: missing colon", "", KindMalformed, 0},
		{"UPSTREAM: <carry> missing colon", "", KindMalformed, 0},
		{"UPSTREAM: <carry>: ", "", KindMalformed, 0},
		{"upstream: <carry>: lowercase", "", KindMalformed, 0},
		{"UPSTREAM: <crary>: typo", "", KindMalformed, 0},
		{"Merge pull request #1 from x/y", "", KindUnclassified, 0},
		{"fix the thing", "", KindUnclassified, 0},
		{"UPSTREAM: <carry>: from the queue", "p_abc", KindQueuedPatch, 0},
	}
	for _, c := range cases {
		k, pr := Classify(c.subject, c.patchID)
		if k != c.kind || pr != c.pr {
			t.Errorf("Classify(%q, %q) = %s, %d; want %s, %d", c.subject, c.patchID, k, pr, c.kind, c.pr)
		}
	}
}

// fixture builds:
//
//	upstream: base ── v1.0 ── (change a.txt) ── v1.1
//	fork:     v1.0 ── M0 (merge -s ours old-fork) ── carries… ── tip
//
// where the old fork branch has a stale carry that must NOT be
// enumerated, and the fork tip has: a carry that conflicts with v1.1, a
// clean carry, a drop, a PR that v1.1 already contains, an unclassified
// commit, a queued-patch commit, a mid-cycle merge of upstream v1.0.1
// (whose upstream commits must not be enumerated), and an evil merge.
type fixture struct {
	t        *testing.T
	r        *runner.Runner
	upstream string
	repo     string
	m0       string
}

func newRunner(t *testing.T) *runner.Runner {
	home := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+home,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	return runner.New(context.Background(), env, nil)
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.r.Output(dir, "git", args...)
	if err != nil {
		f.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func (f *fixture) commit(dir, file, content, msg string, extra ...string) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(dir, "add", "-A")
	f.git(dir, append([]string{"commit", "-q", "-m", msg}, extra...)...)
	return f.git(dir, "rev-parse", "HEAD")
}

func newFixture(t *testing.T) *fixture {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := &fixture{t: t, r: newRunner(t)}
	root := t.TempDir()
	f.upstream = filepath.Join(root, "upstream")
	f.repo = filepath.Join(root, "fork")
	os.MkdirAll(f.upstream, 0o755)

	u := f.upstream
	f.git(u, "init", "-q", "-b", "master")
	f.commit(u, "a.txt", "line1\nline2\nline3\n", "base")
	f.commit(u, "b.txt", "b\n", "upstream b")
	f.git(u, "tag", "v1.0")
	// Patch release branch for the mid-cycle merge.
	f.git(u, "checkout", "-q", "-b", "release-1.0")
	f.commit(u, "c.txt", "c\n", "upstream fix in 1.0.1")
	f.git(u, "tag", "v1.0.1")
	f.git(u, "checkout", "-q", "master")
	f.commit(u, "a.txt", "line1\nUPSTREAM CHANGED\nline3\n", "upstream changes line2")
	f.commit(u, "pr.txt", "pr content\n", "upstream merges PR 777")
	f.git(u, "tag", "v1.1")

	// Fork: old history with a stale carry, then the previous rebase.
	f.git(root, "clone", "-q", u, f.repo)
	r := f.repo
	f.git(r, "checkout", "-q", "-b", "old", "v1.0~1")
	f.commit(r, "stale.txt", "stale\n", "UPSTREAM: <carry>: stale carry from two rebases ago")
	f.git(r, "checkout", "-q", "-b", "fork", "v1.0")
	f.git(r, "merge", "-q", "-s", "ours", "--no-edit", "old")
	f.m0 = f.git(r, "rev-parse", "HEAD")

	f.commit(r, "a.txt", "line1\nFORK CHANGED\nline3\n", "UPSTREAM: <carry>: fork changes line2")
	f.commit(r, "clean.txt", "clean\n", "UPSTREAM: <carry>: clean carry")
	f.commit(r, "gen.txt", "generated\n", "UPSTREAM: <drop>: make update")
	f.commit(r, "pr.txt", "pr content\n", "UPSTREAM: 777: carried upstream PR")
	f.commit(r, "misc.txt", "misc\n", "fix a thing without a prefix")
	f.commit(r, "queued.txt", "queued\n", "UPSTREAM: <carry>: was a queued patch", "--trailer", PatchTrailer+": p_1")
	// Mid-cycle upstream patch-release merge (clean).
	f.git(r, "merge", "-q", "--no-edit", "v1.0.1")
	// Evil merge: a side branch merged with an extra change in the merge.
	f.git(r, "checkout", "-q", "-b", "side")
	f.commit(r, "side.txt", "side\n", "UPSTREAM: <carry>: side branch carry")
	f.git(r, "checkout", "-q", "fork")
	f.git(r, "merge", "-q", "--no-ff", "--no-commit", "side")
	if err := os.WriteFile(filepath.Join(r, "evil.txt"), []byte("only in the merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(r, "add", "evil.txt")
	f.git(r, "commit", "-q", "-m", "Merge side with an extra change")

	// Upstream tags into the namespace the plugin uses.
	f.git(r, "fetch", "-q", u, "+refs/tags/*:"+UpstreamTagsNS+"*")
	return f
}

func TestFindBoundaryAndEnumerate(t *testing.T) {
	f := newFixture(t)
	b, err := FindBoundary(f.r, f.repo, "fork", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Merge != f.m0 {
		t.Fatalf("boundary merge = %s, want %s", b.Merge, f.m0)
	}
	if b.UpstreamBaseTag != "v1.0" {
		t.Errorf("upstream base tag = %q, want v1.0", b.UpstreamBaseTag)
	}

	// An override that isn't a rebase merge is rejected.
	if _, err := FindBoundary(f.r, f.repo, "fork", "fork~1"); err == nil {
		t.Error("expected override pointing at a non-boundary to be rejected")
	}

	commits, err := Enumerate(f.r, f.repo, "fork", b.Merge, UpstreamTagsNS+"v1.1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range commits {
		got = append(got, string(c.Kind)+": "+c.Subject)
	}
	want := []string{
		"carry: UPSTREAM: <carry>: fork changes line2",
		"carry: UPSTREAM: <carry>: clean carry",
		"drop: UPSTREAM: <drop>: make update",
		"pr: UPSTREAM: 777: carried upstream PR",
		"unclassified: fix a thing without a prefix",
		"queued_patch: UPSTREAM: <carry>: was a queued patch",
		"carry: UPSTREAM: <carry>: side branch carry",
		"evil_merge: Merge side with an extra change",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("enumerated:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range commits {
		if c.Kind == KindEvilMerge && !strings.Contains(c.Detail, "only in the merge") {
			t.Errorf("evil merge detail missing its content: %q", c.Detail)
		}
	}
}

func TestPickConflictFingerprintAndResolution(t *testing.T) {
	f := newFixture(t)
	b, err := FindBoundary(f.r, f.repo, "fork", "")
	if err != nil {
		t.Fatal(err)
	}
	commits, err := Enumerate(f.r, f.repo, "fork", b.Merge, UpstreamTagsNS+"v1.1")
	if err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "config", "rerere.enabled", "true")
	f.git(f.repo, "checkout", "-q", "-b", "rebase", UpstreamTagsNS+"v1.1")
	f.git(f.repo, "merge", "-q", "-s", "ours", "--no-edit", "fork")

	// 1. Conflicting carry.
	out, err := CherryPick(f.r, f.repo, commits[0].SHA)
	if err != nil || out != OutcomeConflict {
		t.Fatalf("pick 0: %v %v", out, err)
	}
	fp1, err := Fingerprint(f.r, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	files, err := CaptureConflict(f.r, f.repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "a.txt" || files[0].Ours == nil || !strings.Contains(*files[0].Ours, "UPSTREAM CHANGED") ||
		files[0].Theirs == nil || !strings.Contains(*files[0].Theirs, "FORK CHANGED") || files[0].Merged == nil || !strings.Contains(*files[0].Merged, "<<<<<<<") {
		t.Fatalf("unexpected conflict capture: %+v", files)
	}
	resolved := "line1\nUPSTREAM CHANGED\nFORK CHANGED\nline3\n"
	if err := ApplyResolution(f.r, f.repo, map[string]FileResolution{"a.txt": {Content: &resolved}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Continue(f.r, f.repo); err != nil || !ok {
		t.Fatalf("continue: %v %v", ok, err)
	}
	rr, err := ExportRerere(f.r, f.repo)
	if err != nil || len(rr) != 1 {
		t.Fatalf("rerere export: %v %v", rr, err)
	}

	// 2. Clean carry.
	if out, err := CherryPick(f.r, f.repo, commits[1].SHA); err != nil || out != OutcomePicked {
		t.Fatalf("pick 1: %v %v", out, err)
	}
	// 3. The PR that upstream already merged becomes empty.
	if out, err := CherryPick(f.r, f.repo, commits[3].SHA); err != nil || out != OutcomeEmpty {
		t.Fatalf("pick PR: %v %v", out, err)
	}
	if err := Skip(f.r, f.repo); err != nil {
		t.Fatal(err)
	}

	// Replay from scratch on a fresh branch: same conflict → same
	// fingerprint; rerere (imported into a fresh clone) resolves it.
	f.git(f.repo, "checkout", "-q", "-b", "rebase2", UpstreamTagsNS+"v1.1")
	f.git(f.repo, "merge", "-q", "-s", "ours", "--no-edit", "fork")
	if out, err := CherryPick(f.r, f.repo, commits[0].SHA); err != nil || out != OutcomeConflict {
		t.Fatalf("replay pick 0: %v %v", out, err)
	}
	fp2, err := Fingerprint(f.r, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable across replays: %s vs %s", fp1, fp2)
	}
	files, err = CaptureConflict(f.r, f.repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Suggestion == nil || *files[0].Suggestion != resolved {
		t.Fatalf("expected rerere suggestion %q, got %+v", resolved, files[0].Suggestion)
	}
	if files[0].Merged == nil || !strings.Contains(*files[0].Merged, "<<<<<<<") {
		t.Fatalf("expected markers regenerated in Merged, got %v", files[0].Merged)
	}
	// Auto mode: rerere's resolution staged without a human.
	if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := StageRerereResolved(f.r, f.repo)
	if err != nil || !done {
		t.Fatalf("stage rerere: %v %v", done, err)
	}
	if ok, err := Continue(f.r, f.repo); err != nil || !ok {
		t.Fatalf("continue replay: %v %v", ok, err)
	}
}

func TestImportRerere(t *testing.T) {
	f := newFixture(t)
	if err := ImportRerere(f.r, f.repo, []RerereEntry{{ID: "abc123", Files: map[string]string{"preimage": "x", "postimage": "y"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := ExportRerere(f.r, f.repo)
	if err != nil || len(got) != 1 || got[0].Files["postimage"] != "y" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}

// Forks that track upstream main rather than releases: the previous
// rebase sits on an untagged upstream commit, and the fork later merged
// newer upstream main. Both must be recognised as upstream content via
// refs/upstream/heads/*, not just tags.
func TestBoundaryOnUntaggedUpstreamBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := &fixture{t: t, r: newRunner(t)}
	root := t.TempDir()
	u, r := filepath.Join(root, "upstream"), filepath.Join(root, "fork")
	os.MkdirAll(u, 0o755)
	f.git(u, "init", "-q", "-b", "main")
	f.commit(u, "a.txt", "a\n", "base")
	f.git(u, "tag", "v0.1.0")
	f.commit(u, "b.txt", "b\n", "untagged main commit the fork rebased onto")

	f.git(root, "clone", "-q", u, r)
	f.git(r, "checkout", "-q", "-b", "old", "v0.1.0")
	f.commit(r, "old.txt", "old\n", "UPSTREAM: <carry>: carry from before")
	f.git(r, "checkout", "-q", "-b", "master", "origin/main")
	f.git(r, "merge", "-q", "-s", "ours", "--no-edit", "old")
	m0 := f.git(r, "rev-parse", "HEAD")
	f.commit(r, "carry.txt", "c\n", "UPSTREAM: <carry>: the carry")

	// Upstream main moves on; the fork merges it mid-cycle.
	f.commit(u, "c.txt", "c\n", "newer upstream main commit")
	f.git(r, "fetch", "-q", "origin")
	f.git(r, "merge", "-q", "--no-edit", "origin/main")

	// Tags alone: the boundary isn't upstream-reachable (the old bug).
	f.git(r, "fetch", "-q", u, "+refs/tags/*:"+UpstreamTagsNS+"*")
	if _, err := FindBoundary(f.r, r, "master", ""); err == nil {
		t.Fatal("expected tags-only detection to fail for an untagged base")
	}

	f.git(r, "fetch", "-q", u, "+refs/heads/*:"+UpstreamHeadsNS+"*")
	b, err := FindBoundary(f.r, r, "master", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Merge != m0 {
		t.Fatalf("boundary = %s, want %s", b.Merge, m0)
	}
	if _, err := FindBoundary(f.r, r, "master", m0); err != nil {
		t.Fatalf("explicit override rejected: %v", err)
	}
	commits, err := Enumerate(f.r, r, "master", b.Merge, UpstreamHeadsNS+"main")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Subject != "UPSTREAM: <carry>: the carry" {
		t.Fatalf("enumerated %+v, want only the carry", commits)
	}
}
