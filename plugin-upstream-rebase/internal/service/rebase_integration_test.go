package service

// End-to-end exercise of the rebase action against real git repos and
// the plugin's real Postgres (docker compose's rebase-postgres). Skipped
// when the database isn't reachable. Override with REBASE_TEST_PG_DSN.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/gitrebase"
	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
)

type fakeStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*pluginpb.ExecuteEvent
}

func (f *fakeStream) Send(e *pluginpb.ExecuteEvent) error { f.events = append(f.events, e); return nil }
func (f *fakeStream) Context() context.Context            { return f.ctx }

func (f *fakeStream) terminal() *pluginpb.ExecuteEvent {
	if len(f.events) == 0 {
		return nil
	}
	return f.events[len(f.events)-1]
}

type env struct {
	t                         *testing.T
	r                         *runner.Runner
	svc                       *Rebaser
	upstream, fork, push, tmp string
	workflowID                string
	instanceCfg               []byte
}

func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.r.Output(dir, "git", args...)
	if err != nil {
		e.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func (e *env) commit(dir, file, content, msg string) {
	e.t.Helper()
	full := filepath.Join(dir, file)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", msg)
}

func testKey(t *testing.T) string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

func newEnv(t *testing.T) *env {
	dsn := os.Getenv("REBASE_TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://rebase:rebase@localhost:5434/rebase?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("plugin Postgres not reachable (%v); run `docker compose up -d rebase-postgres`", err)
	}
	t.Cleanup(st.Close)

	home := t.TempDir()
	r := runner.New(context.Background(), append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+home,
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com"), nil)
	tmp := t.TempDir()
	e := &env{
		t: t, r: r, tmp: tmp,
		svc:        &Rebaser{Store: st, WorkRoot: filepath.Join(tmp, "work")},
		workflowID: fmt.Sprintf("it-%d", time.Now().UnixNano()),
	}
	cfg, _ := json.Marshal(map[string]any{
		"git_user_name": "Rebase Bot", "git_user_email": "bot@example.com", "ssh_private_key": testKey(t),
	})
	e.instanceCfg = cfg

	// Upstream: v1.0 → v1.1 (changes line2 of a.txt, merges "PR 777").
	up := filepath.Join(tmp, "upstream-wt")
	os.MkdirAll(up, 0o755)
	e.git(up, "init", "-q", "-b", "master")
	e.commit(up, "a.txt", "line1\nline2\nline3\n", "base")
	e.commit(up, "Makefile", "update:\n\tdate +%s > /dev/null; echo generated > generated.txt\n", "upstream Makefile")
	e.git(up, "tag", "-a", "-m", "v1.0", "v1.0")
	e.commit(up, "a.txt", "line1\nUPSTREAM CHANGED\nline3\n", "upstream changes line2")
	e.commit(up, "pr.txt", "pr content\n", "upstream merges PR 777")
	e.git(up, "tag", "-a", "-m", "v1.1", "v1.1")
	e.upstream = filepath.Join(tmp, "upstream.git")
	e.git(tmp, "clone", "-q", "--bare", up, e.upstream)

	// Fork: previous rebase onto v1.0, then carries.
	fk := filepath.Join(tmp, "fork-wt")
	e.git(tmp, "clone", "-q", e.upstream, fk)
	e.git(fk, "checkout", "-q", "-b", "old", "v1.0~1")
	e.commit(fk, "stale.txt", "stale\n", "UPSTREAM: <carry>: stale carry from an older rebase")
	e.git(fk, "checkout", "-q", "-B", "master", "v1.0")
	e.git(fk, "merge", "-q", "-s", "ours", "--no-edit", "old")
	e.commit(fk, "a.txt", "line1\nFORK CHANGED\nline3\n", "UPSTREAM: <carry>: fork changes line2")
	e.commit(fk, "clean.txt", "clean\n", "UPSTREAM: <carry>: clean carry")
	e.commit(fk, "generated.txt", "old generated\n", "UPSTREAM: <drop>: make update")
	e.commit(fk, "pr.txt", "pr content\n", "UPSTREAM: 777: carried upstream PR")
	e.commit(fk, "misc.txt", "misc\n", "fix a thing without a prefix")
	e.fork = filepath.Join(tmp, "fork.git")
	e.git(tmp, "clone", "-q", "--bare", fk, e.fork)
	e.git(e.fork, "branch", "-D", "old")

	e.push = filepath.Join(tmp, "push.git")
	e.git(tmp, "init", "-q", "--bare", e.push)
	return e
}

func (e *env) params(target, fork, push string) []byte {
	b, _ := json.Marshal(map[string]any{
		"clone_url": fork, "fork_branch": "master", "upstream_url": e.upstream, "target_ref": target,
		"push_url": push, "rerere": "suggest",
		"dirs": []map[string]any{{"path": ".", "make_targets": []string{"update"}}},
		"commands": []map[string]any{
			{"name": "write cmd file", "run": `echo "$((1+1))" > cmd.txt`},
			{"name": "no-op", "run": "true"},
		},
	})
	return b
}

func (e *env) exec(runID, action string, payload []byte, resolution []byte, params []byte) *fakeStream {
	e.t.Helper()
	fs := &fakeStream{ctx: context.Background()}
	if action == "rebase" {
		payload = params
	}
	req := &pluginpb.ExecuteRequest{
		Action: action, Payload: payload, InstanceId: "it", InstanceConfig: e.instanceCfg,
		WorkflowRunId: runID, WorkflowId: e.workflowID, StepId: "rebase", Attempt: 1,
		IdempotencyKey: runID + "/rebase", Resolution: resolution,
	}
	if err := e.svc.Execute(req, fs); err != nil {
		e.t.Fatalf("%s %s: %v", action, runID, err)
	}
	return fs
}

func suspendPromptOf(t *testing.T, fs *fakeStream) suspendPrompt {
	t.Helper()
	s, ok := fs.terminal().GetEvent().(*pluginpb.ExecuteEvent_Suspend)
	if !ok {
		t.Fatalf("expected Suspend, got %T", fs.terminal().GetEvent())
	}
	var p suspendPrompt
	if err := json.Unmarshal(s.Suspend.GetPrompt(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func finalOutputs(t *testing.T, fs *fakeStream) map[string][]byte {
	t.Helper()
	f, ok := fs.terminal().GetEvent().(*pluginpb.ExecuteEvent_Final)
	if !ok {
		t.Fatalf("expected Final, got %T", fs.terminal().GetEvent())
	}
	return f.Final.GetOutputs().GetValues()
}

func (e *env) table(runID string) map[string]store.CommitRow {
	rows, err := e.svc.Store.ListCommits(context.Background(), runID, "rebase")
	if err != nil {
		e.t.Fatal(err)
	}
	m := map[string]store.CommitRow{}
	for _, r := range rows {
		m[r.Subject] = r
	}
	return m
}

func TestRebaseEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cont := []byte(`{"action":"continue"}`)
	params := e.params("v1.1", e.fork, e.push)

	// ── Run 1 ────────────────────────────────────────────────────────────
	run1 := e.workflowID + "-run1"

	// 1. The unprefixed commit needs a decision before anything is picked.
	p := suspendPromptOf(t, e.exec(run1, "rebase", nil, nil, params))
	if p.Kind != "decide" || p.Pending != 1 {
		t.Fatalf("first suspend = %+v, want decide/1", p)
	}
	tbl := e.table(run1)
	if tbl["UPSTREAM: <carry>: stale carry from an older rebase"].Subject != "" {
		t.Fatal("stale carry from before the previous rebase was enumerated")
	}
	misc := tbl["fix a thing without a prefix"]
	if misc.Status != stAwaitingDecision || misc.Kind != "unclassified" {
		t.Fatalf("misc row = %+v", misc)
	}
	dec, _ := json.Marshal(map[string]any{"decisions": []map[string]string{{
		"orig_sha": misc.OrigSHA, "question": "classify", "decision": "carry", "reason": "real fork change, missing prefix",
	}}})
	e.exec(run1, "decisions_save", dec, nil, nil)

	// 2. Lose the working copy: resume must replay from scratch and
	//    still reach the same conflict.
	if err := os.RemoveAll(e.svc.WorkRoot); err != nil {
		t.Fatal(err)
	}
	p = suspendPromptOf(t, e.exec(run1, "rebase", nil, cont, params))
	if p.Kind != "conflict" || p.ConflictID == "" {
		t.Fatalf("second suspend = %+v, want conflict", p)
	}
	got := e.exec(run1, "conflict_get", []byte(fmt.Sprintf(`{"conflict_id":%q}`, p.ConflictID)), nil, nil)
	var cg struct {
		Conflict store.Conflict `json:"conflict"`
	}
	if err := json.Unmarshal(got.terminal().GetFinal().GetResult(), &cg); err != nil {
		t.Fatal(err)
	}
	var files []gitrebase.ConflictFile
	_ = json.Unmarshal(cg.Conflict.Files, &files)
	if len(files) != 1 || files[0].Path != "a.txt" {
		t.Fatalf("conflict files = %+v", files)
	}

	// Saving with markers left in is refused.
	bad, _ := json.Marshal(map[string]any{"conflict_id": p.ConflictID, "kind": "files",
		"files": map[string]any{"a.txt": map[string]string{"content": *files[0].Merged}}})
	fs := &fakeStream{ctx: ctx}
	if err := e.svc.Execute(&pluginpb.ExecuteRequest{Action: "resolution_save", Payload: bad, WorkflowId: e.workflowID, StepId: "rebase"}, fs); err == nil {
		t.Fatal("resolution with conflict markers was accepted")
	}

	resolved := "line1\nUPSTREAM CHANGED\nFORK CHANGED\nline3\n"
	good, _ := json.Marshal(map[string]any{"conflict_id": p.ConflictID, "kind": "files",
		"files": map[string]any{"a.txt": map[string]string{"content": resolved}}})
	e.exec(run1, "resolution_save", good, nil, nil)

	// A user-defined commit in the patch queue.
	patch := "diff --git a/extra.txt b/extra.txt\nnew file mode 100644\n--- /dev/null\n+++ b/extra.txt\n@@ -0,0 +1 @@\n+extra\n"
	pp, _ := json.Marshal(map[string]any{"name": "add extra", "target_dir": ".", "content": patch})
	e.exec(run1, "patches_upsert", pp, nil, nil)

	// 3. Resume in place → finish.
	out := finalOutputs(t, e.exec(run1, "rebase", nil, cont, params))
	var commit string
	_ = json.Unmarshal(out["commit"], &commit)
	if pushed := e.git(e.push, "rev-parse", "rebase/v1-1"); pushed != commit {
		t.Fatalf("pushed %s, output commit %s", pushed, commit)
	}
	tbl = e.table(run1)
	want := map[string]string{
		"UPSTREAM: <carry>: fork changes line2": stResolved,
		"UPSTREAM: <carry>: clean carry":        stPicked,
		"UPSTREAM: <drop>: make update":         stDropped,
		"UPSTREAM: 777: carried upstream PR":    stAlreadyUpstream,
		"fix a thing without a prefix":          stPicked,
	}
	for subj, st := range want {
		if tbl[subj].Status != st {
			t.Errorf("%q status = %s, want %s", subj, tbl[subj].Status, st)
		}
	}
	if len(tbl) != len(want) {
		t.Errorf("table has %d rows, want %d", len(tbl), len(want))
	}
	if src := tbl["UPSTREAM: <carry>: fork changes line2"].ResolutionSource; src != "human" {
		t.Errorf("resolution source = %q, want human", src)
	}
	log := e.git(e.push, "log", "--format=%s", "rebase/v1-1")
	for _, s := range []string{
		`UPSTREAM: <drop>: make update`,             // regenerated by the make phase
		`UPSTREAM: <drop>: apply patch "add extra"`, // patch queue
		"fix a thing without a prefix",
		"UPSTREAM: <carry>: clean carry",
		"UPSTREAM: <carry>: fork changes line2",
	} {
		if !strings.Contains(log, s) {
			t.Errorf("pushed branch is missing %q:\n%s", s, log)
		}
	}
	// Shell commands commit last, after the make targets; one that
	// changes nothing gets no commit.
	if top := strings.SplitN(log, "\n", 3); len(top) < 2 ||
		top[0] != "UPSTREAM: <drop>: write cmd file" || top[1] != "UPSTREAM: <drop>: make update" {
		t.Errorf("branch tip = %q, want the command commit on top of the make commit", top)
	}
	// The plain-diff patch's scratch file must not ride along into the
	// patch commit (`git add -A` stages everything in the worktree). The
	// final tree alone would hide it: the next `git add -A` commit (make,
	// commands) records its deletion.
	patchCommit := e.git(e.push, "log", "-1", "--format=%H", "--grep=apply patch", "rebase/v1-1")
	if files := e.git(e.push, "show", "--name-only", "--format=", patchCommit); files != "extra.txt" {
		t.Errorf("patch commit touches %q, want only extra.txt", files)
	}
	if strings.Contains(log, "no-op") {
		t.Errorf("command with no changes was committed:\n%s", log)
	}
	if content := e.git(e.push, "show", "rebase/v1-1:cmd.txt"); content != "2" {
		t.Errorf("cmd.txt = %q", content)
	}
	if content := e.git(e.push, "show", "rebase/v1-1:a.txt"); content+"\n" != resolved {
		t.Errorf("a.txt = %q", content)
	}
	if tr := e.git(e.push, "log", "-1", "--format=%(trailers:key=Rebase-Patch-Id,valueonly)", "--grep=apply patch", "rebase/v1-1"); !strings.HasPrefix(tr, "p_") {
		t.Errorf("patch commit trailer = %q", tr)
	}
	// The conflict was answered via suspend → resume in place; rerere
	// must still have recorded it (the resume-time prune must not drop
	// the in-progress preimage).
	if rrNow, _ := e.svc.Store.ListRerereMeta(ctx, e.workflowID, "rebase"); len(rrNow) == 0 {
		t.Error("no rerere entry recorded for the conflict resolved in run 1")
	}
	run, _ := e.svc.Store.GetRun(ctx, run1, "rebase")
	if run.Status != "succeeded" || !strings.Contains(run.RangeDiff, "fork changes line2") {
		t.Errorf("run = %s, range-diff %q", run.Status, run.RangeDiff)
	}

	// ── Run 2: same pipeline again - decision and resolution reused, no
	//    human needed.
	run2 := e.workflowID + "-run2"
	out = finalOutputs(t, e.exec(run2, "rebase", nil, nil, params))
	if src := e.table(run2)["UPSTREAM: <carry>: fork changes line2"].ResolutionSource; src != "reused" {
		t.Errorf("run 2 resolution source = %q, want reused", src)
	}

	// ── Next release: rebase the pushed result onto v1.2. The plugin's
	//    own generated commits are dropped and the queued patch is
	//    re-applied from the queue, never picked a second time.
	up := filepath.Join(e.tmp, "upstream-wt")
	e.commit(up, "b.txt", "b\n", "upstream 1.2 change")
	e.git(up, "tag", "-a", "-m", "v1.2", "v1.2")
	e.git(up, "push", "-q", e.upstream, "master", "--tags")
	e.git(e.push, "branch", "-f", "master", "rebase/v1-1")
	run3 := e.workflowID + "-run3"
	push2 := filepath.Join(e.tmp, "push2.git")
	e.git(e.tmp, "init", "-q", "--bare", push2)
	finalOutputs(t, e.exec(run3, "rebase", nil, nil, e.params("v1.2", e.push, push2)))
	tbl = e.table(run3)
	if row := tbl[`UPSTREAM: <drop>: apply patch "add extra"`]; row.Status != stRequeuedPatch {
		t.Errorf("queued patch row = %+v", row)
	}
	if row := tbl["UPSTREAM: <drop>: make update"]; row.Status != stDropped {
		t.Errorf("generated make commit row = %+v", row)
	}
	if row := tbl["UPSTREAM: <drop>: write cmd file"]; row.Status != stDropped {
		t.Errorf("generated command commit row = %+v", row)
	}
	e.git(push2, "fetch", "-q", e.push, "rebase/v1-1:rebase/v1-1")
	if n := strings.Count(e.git(push2, "log", "--format=%s", "rebase/v1-2"), `apply patch "add extra"`); n != 2 {
		// one from the v1.1 history (behind the -s ours merge), one re-applied
		t.Errorf("patch commit appears %d times in history, want 2 (old + re-applied)", n)
	}
	if n := strings.Count(e.git(push2, "log", "--format=%s", "rebase/v1-2", "^rebase/v1-1"), `apply patch "add extra"`); n != 1 {
		t.Errorf("patch applied %d times in the new rebase, want exactly 1", n)
	}

	// ── Memory: rerere entries are linked to the resolution that
	//    produced them; deleting it deletes them; a workflow-wide wipe
	//    clears everything.
	type rrList struct {
		Entries []store.RerereEntry `json:"entries"`
	}
	var rl rrList
	_ = json.Unmarshal(e.exec(run1, "rerere_list", nil, nil, nil).terminal().GetFinal().GetResult(), &rl)
	if len(rl.Entries) == 0 {
		t.Fatal("expected rerere entries after resolving a conflict in suggest mode")
	}
	linked := rl.Entries[0]
	if linked.ResolutionID == "" || linked.Path != "a.txt" || !strings.Contains(linked.Subject, "fork changes line2") {
		t.Fatalf("rerere entry not linked: %+v", linked)
	}
	del, _ := json.Marshal(map[string]any{"resolution_ids": []string{linked.ResolutionID}})
	var delOut struct {
		Deleted map[string]int64 `json:"deleted"`
	}
	_ = json.Unmarshal(e.exec(run1, "memory_delete", del, nil, nil).terminal().GetFinal().GetResult(), &delOut)
	if delOut.Deleted["resolutions"] != 1 || delOut.Deleted["rerere"] < 1 {
		t.Fatalf("cascade delete = %+v", delOut.Deleted)
	}

	wipe, _ := json.Marshal(map[string]any{"scope": "workflow", "categories": map[string]bool{
		"resolutions": true, "decisions": true, "rerere": true, "patches": true, "run_history": true}})
	_ = json.Unmarshal(e.exec(run1, "memory_wipe", wipe, nil, nil).terminal().GetFinal().GetResult(), &delOut)
	if delOut.Deleted["decisions"] < 1 || delOut.Deleted["patches"] < 1 || delOut.Deleted["run_history"] < 3 {
		t.Fatalf("wipe counts = %+v", delOut.Deleted)
	}
	if res, _ := e.svc.Store.ListResolutions(ctx, e.workflowID, "rebase", ""); len(res) != 0 {
		t.Errorf("resolutions left after wipe: %d", len(res))
	}
	if left, _ := e.svc.Store.ListRerereMeta(ctx, e.workflowID, "rebase"); len(left) != 0 {
		t.Errorf("rerere entries left after wipe: %d", len(left))
	}
	if r, _ := e.svc.Store.GetRun(ctx, run1, "rebase"); r != nil {
		t.Error("finished run survived run-history wipe")
	}
}

// Editing a patch (the editor sends no ordinal) must keep its place in
// the queue; patch order is semantic.
func TestPatchEditKeepsPosition(t *testing.T) {
	e := newEnv(t)
	run := e.workflowID + "-patches"
	type patchesOut struct {
		Patches []struct {
			PatchID string `json:"patch_id"`
			Name    string `json:"name"`
		} `json:"patches"`
	}
	upsert := func(body map[string]any) {
		b, _ := json.Marshal(body)
		e.exec(run, "patches_upsert", b, nil, nil)
	}
	list := func() patchesOut {
		var out patchesOut
		_ = json.Unmarshal(e.exec(run, "patches_list", nil, nil, nil).terminal().GetFinal().GetResult(), &out)
		return out
	}
	for _, n := range []string{"first", "second", "third"} {
		upsert(map[string]any{"name": n, "target_dir": ".", "content": "diff --git a/x b/x\n"})
	}
	before := list()
	if len(before.Patches) != 3 {
		t.Fatalf("got %d patches", len(before.Patches))
	}
	upsert(map[string]any{"patch_id": before.Patches[2].PatchID, "name": "third (edited)", "target_dir": ".", "content": "diff --git a/y b/y\n"})
	var got []string
	for _, p := range list().Patches {
		got = append(got, p.Name)
	}
	if strings.Join(got, ",") != "first,second,third (edited)" {
		t.Errorf("order after edit = %v", got)
	}
}

// A carry that upstream absorbed verbatim becomes empty: the run must
// stop and ask (never silently drop a carry), and a keep_empty decision
// must produce an empty commit with the original message.
func TestRebaseEmptyCarryNeedsDecision(t *testing.T) {
	e := newEnv(t)
	cont := []byte(`{"action":"continue"}`)
	wt := filepath.Join(e.tmp, "extra-wt")
	e.git(e.tmp, "clone", "-q", e.fork, wt)
	e.commit(wt, "absorbed.txt", "absorbed\n", "UPSTREAM: <carry>: carry that upstream also merged")
	e.git(wt, "push", "-q", "origin", "master")
	up := filepath.Join(e.tmp, "upstream-wt")
	e.commit(up, "absorbed.txt", "absorbed\n", "upstream absorbs the carry")
	e.git(up, "tag", "-a", "-m", "v1.1.1", "v1.1.1")
	e.git(up, "push", "-q", e.upstream, "master", "--tags")
	params := e.params("v1.1.1", e.fork, e.push)
	run := e.workflowID + "-empty"

	p := suspendPromptOf(t, e.exec(run, "rebase", nil, nil, params))
	misc := e.table(run)["fix a thing without a prefix"]
	dec, _ := json.Marshal(map[string]any{"decisions": []map[string]string{{
		"orig_sha": misc.OrigSHA, "question": "classify", "decision": "drop", "reason": "scratch commit",
	}}})
	e.exec(run, "decisions_save", dec, nil, nil)

	p = suspendPromptOf(t, e.exec(run, "rebase", nil, cont, params))
	if p.Kind != "conflict" {
		t.Fatalf("want conflict, got %+v", p)
	}
	good, _ := json.Marshal(map[string]any{"conflict_id": p.ConflictID, "kind": "skip", "reason": "superseded upstream"})
	e.exec(run, "resolution_save", good, nil, nil)

	p = suspendPromptOf(t, e.exec(run, "rebase", nil, cont, params))
	row := e.table(run)["UPSTREAM: <carry>: carry that upstream also merged"]
	if p.Kind != "decide" || row.Status != stAwaitingDecision {
		t.Fatalf("want empty-carry decision, got prompt %+v row %+v", p, row)
	}
	dec, _ = json.Marshal(map[string]any{"decisions": []map[string]string{{
		"orig_sha": row.OrigSHA, "question": "empty", "decision": "keep_empty", "reason": "keep the marker commit",
	}}})
	e.exec(run, "decisions_save", dec, nil, nil)

	finalOutputs(t, e.exec(run, "rebase", nil, cont, params))
	tbl := e.table(run)
	if st := tbl["UPSTREAM: <carry>: carry that upstream also merged"].Status; st != stKeptEmpty {
		t.Errorf("empty carry status = %s", st)
	}
	if st := tbl["fix a thing without a prefix"].Status; st != stDropped {
		t.Errorf("dropped unclassified status = %s", st)
	}
	if st := tbl["UPSTREAM: <carry>: fork changes line2"].Status; st != stSkipped {
		t.Errorf("skipped-by-resolution status = %s", st)
	}
	if !strings.Contains(e.git(e.push, "log", "--format=%s", "rebase/v1-1-1"), "carry that upstream also merged") {
		t.Error("kept-empty commit missing from pushed branch")
	}
}
