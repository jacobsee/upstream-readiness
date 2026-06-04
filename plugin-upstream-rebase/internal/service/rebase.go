package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/gitrebase"
	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/cmdlog"
	"github.com/jacobsee/upstream-readiness/pluginkit/execstream"
	"github.com/jacobsee/upstream-readiness/pluginkit/gitenv"
	"github.com/jacobsee/upstream-readiness/pluginkit/godeps"
	"github.com/jacobsee/upstream-readiness/pluginkit/patchqueue"
	"github.com/jacobsee/upstream-readiness/pluginkit/refs"
	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stepParams is the `rebase` action's params blob. Opaque to the
// orchestrator; any leaf may be an {"__ref": {...}} marker.
type stepParams struct {
	// CloneURL is the fork repository whose branch carries the UPSTREAM
	// commits (read-only for us).
	CloneURL string `json:"clone_url"`
	// ForkBranch is the branch on CloneURL holding the previous rebase
	// plus everything carried since. Default "master".
	ForkBranch string `json:"fork_branch"`
	// UpstreamURL is the upstream project the fork tracks.
	UpstreamURL string `json:"upstream_url"`
	// TargetRef is the upstream tag (or ref / SHA) to rebase onto.
	TargetRef string `json:"target_ref"`
	// PreviousRebaseCommit overrides auto-detection of the previous
	// `merge -s ours` commit. Verified, not trusted.
	PreviousRebaseCommit string `json:"previous_rebase_commit"`
	// PushURL / BranchName: where the result goes. PushURL defaults to
	// the instance's fork_url_template; BranchName to rebase/<target>.
	PushURL    string `json:"push_url"`
	BranchName string `json:"branch_name"`
	// Rerere is "off" | "suggest" | "auto" (default "suggest"). Exact
	// saved resolutions are always auto-applied regardless.
	Rerere string `json:"rerere"`
	// Dirs runs the generic-go-dep pipeline (bumps, tidy, vendor, patch
	// queue, make) after all commits are re-applied. Optional; updates
	// may be empty per dir.
	Dirs []godeps.Dir `json:"dirs"`
	// Commands are bare shell commands run in order after everything
	// else that commits (re-picks, then every dir's dependency pipeline
	// including its make targets). Each one's changes land in their own
	// commit. Optional.
	Commands []shellCommand `json:"commands"`
}

// shellCommand is one entry of stepParams.Commands.
type shellCommand struct {
	// Name is the commit subject, after the UPSTREAM: <drop>: prefix.
	Name string `json:"name"`
	// Run is handed to `sh -c`.
	Run string `json:"run"`
	// Dir is the working directory relative to the repo root. Default ".".
	Dir string `json:"dir"`
}

// normalizeCommands validates resolved commands and cleans each dir.
func normalizeCommands(cmds []shellCommand) error {
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

// Row statuses. "pending", "conflict", and "awaiting_decision" are
// non-terminal; the run cannot finish while any row holds one.
const (
	stPending          = "pending"
	stAwaitingDecision = "awaiting_decision"
	stConflict         = "conflict"
	stPicked           = "picked"
	stResolved         = "resolved"
	stAlreadyUpstream  = "already_upstream"
	stDropped          = "dropped"
	stKeptEmpty        = "kept_empty"
	stSkipped          = "skipped"
	stRequeuedPatch    = "requeued_patch"
	stAcknowledged     = "acknowledged"
)

func terminal(status string) bool {
	switch status {
	case stPending, stAwaitingDecision, stConflict:
		return false
	}
	return true
}

// Decision questions.
const (
	qClassify = "classify" // unclassified / malformed / evil merge: carry or drop?
	qEmpty    = "empty"    // a carry became empty: drop or keep_empty?
)

// resolutionBlob is what this plugin's resolution slot hands back
// through the orchestrator. The actual answers (resolutions, decisions)
// are saved to the plugin's DB by the slot before it resolves; the blob
// only says whether to continue or give up.
type resolutionBlob struct {
	Action string `json:"action"` // continue | abort
	Reason string `json:"reason,omitempty"`
}

// progressPayload is the step-runtime slot's input. table_rev changes
// whenever the commit table does, so the slot knows to re-fetch it via
// run_get instead of the payload carrying hundreds of rows.
type progressPayload struct {
	Phase    string         `json:"phase"`
	Message  string         `json:"message,omitempty"`
	Log      string         `json:"log,omitempty"`
	Counts   map[string]int `json:"counts,omitempty"`
	Total    int            `json:"total,omitempty"`
	Current  *currentCommit `json:"current,omitempty"`
	TableRev int64          `json:"table_rev,omitempty"`
}

type currentCommit struct {
	Ordinal int    `json:"ordinal"`
	Subject string `json:"subject"`
}

// suspendPrompt is the opaque prompt for the resolution slot.
type suspendPrompt struct {
	Kind       string `json:"kind"` // decide | conflict
	ConflictID string `json:"conflict_id,omitempty"`
	Ordinal    int    `json:"ordinal,omitempty"`
	Subject    string `json:"subject,omitempty"`
	Pending    int    `json:"pending,omitempty"`
}

// errSuspended unwinds the job after it sent ExecuteSuspend.
var errSuspended = errors.New("suspended")

// job is one execution of the rebase action.
type job struct {
	s      *Rebaser
	ctx    context.Context
	req    *pluginpb.ExecuteRequest
	stream pluginpb.Plugin_ExecuteServer

	cfg             gitenv.InstanceConfig
	p               stepParams
	pushURL         string
	dirAutoReplaces [][]*refs.AutoReplaceDirective

	run       *store.Run
	rows      []store.CommitRow
	r         *runner.Runner
	rec       *cmdlog.Recorder // shared by every runner this execution creates
	workDir   string
	repoDir   string
	phase     string
	current   *currentCommit
	targetRef string // local ref name for the target
}

func (s *Rebaser) handleRebase(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	j := &job{s: s, ctx: ctx, req: req, stream: stream}
	if s.Store != nil {
		j.rec = cmdlog.NewRecorder(ctx, s.Store.Commands, req)
	}
	if err := j.prepare(); err != nil {
		return err
	}
	err := j.execute()
	if errors.Is(err, errSuspended) {
		return nil
	}
	if err != nil {
		j.fail(err)
		return err
	}
	return nil
}

// prepare decodes and validates everything that doesn't touch git.
func (j *job) prepare() error {
	if j.s.Store == nil {
		return status.Error(codes.FailedPrecondition, "plugin store is not configured; set --pg-dsn or PG_DSN")
	}
	if !patchqueue.InWorkflow(j.req) || j.req.GetWorkflowRunId() == "" || j.req.GetWorkflowRunId() == "ad-hoc" {
		return status.Error(codes.FailedPrecondition, "rebase runs only inside a workflow run (it needs run/step scoping to suspend and to key its state)")
	}
	if blob := j.req.GetResolution(); len(blob) > 0 && string(blob) != "null" {
		var rb resolutionBlob
		if err := json.Unmarshal(blob, &rb); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode resolution: %v", err)
		}
		if rb.Action == "abort" {
			msg := "rebase aborted from the resolution panel"
			if rb.Reason != "" {
				msg += ": " + rb.Reason
			}
			return status.Error(codes.FailedPrecondition, msg)
		}
	}
	if blob := j.req.GetInstanceConfig(); len(blob) > 0 {
		if err := json.Unmarshal(blob, &j.cfg); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode instance config: %v", err)
		}
	}
	if err := gitenv.CheckForExecute(j.req.GetInstanceId(), j.cfg); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	var rawParams map[string]any
	if raw := j.req.GetPayload(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &rawParams); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode step params: %v", err)
		}
	}
	resolved, err := refs.ResolveRefs(j.req.GetPayload(), j.req.GetUpstreamOutputs())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "resolve upstream references: %v", err)
	}
	if err := json.Unmarshal(resolved, &j.p); err != nil {
		return status.Errorf(codes.InvalidArgument, "decode step params: %v", err)
	}
	p := &j.p
	if p.CloneURL == "" || p.UpstreamURL == "" || p.TargetRef == "" {
		return status.Error(codes.InvalidArgument, "clone_url, upstream_url, and target_ref are required")
	}
	if p.ForkBranch == "" {
		p.ForkBranch = "master"
	}
	if p.Rerere == "" {
		p.Rerere = "suggest"
	}
	if !validRerere(p.Rerere) {
		return status.Error(codes.InvalidArgument, `rerere must be one of "off", "suggest", "auto"`)
	}
	if p.BranchName == "" {
		p.BranchName = "rebase/" + gitenv.Slugify(p.TargetRef)
	}
	if len(p.Dirs) > 0 {
		if err := godeps.Normalize(p.Dirs, j.cfg.DefaultMakeTargets, godeps.Options{AllowNoUpdates: true}); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if err := normalizeCommands(p.Commands); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	j.pushURL, err = gitenv.ResolvePushURL(p.PushURL, p.CloneURL, j.cfg.ForkURLTemplate)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if j.pushURL == p.CloneURL {
		return status.Errorf(codes.InvalidArgument, "refusing to push to the clone source %q - set a fork URL via push_url or instance fork_url_template", p.CloneURL)
	}
	j.dirAutoReplaces, err = godeps.DetectAutoReplaces(rawParams, p.Dirs, j.req.GetUpstreamOutputs())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	j.workDir = j.s.stepWorkDir(j.req.GetWorkflowRunId(), j.req.GetStepId())
	return nil
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func safeName(s string) string { return unsafeChars.ReplaceAllString(s, "_") }

// execute runs (or resumes) the rebase. Returns errSuspended after
// sending ExecuteSuspend.
func (j *job) execute() error {
	existing, err := j.s.Store.GetRun(j.ctx, j.req.GetWorkflowRunId(), j.req.GetStepId())
	if err != nil {
		return fmt.Errorf("load run state: %w", err)
	}
	resume := false
	if existing != nil && existing.Status == "suspended" && existing.WorkDir == j.workDir {
		if err := j.setupEnv(false); err != nil {
			return err
		}
		head, herr := gitrebase.RevParse(j.r, j.repoDir, "HEAD")
		if herr == nil && head == existing.CheckpointHead {
			resume = true
		}
	}

	if resume {
		j.run = existing
		j.run.Status = "running"
		j.run.Error = ""
		j.targetRef = targetLocalRef
		j.rows, err = j.s.Store.ListCommits(j.ctx, j.run.WorkflowRunID, j.run.StepID)
		if err != nil {
			return err
		}
		j.r.Note(fmt.Sprintf("# resuming from checkpoint %s\n", j.run.CheckpointHead))
		j.emit("resuming", "resuming from checkpoint")
		if err := j.syncRerere(); err != nil {
			return err
		}
	} else {
		if existing != nil {
			j.emit("preparing", "previous working copy unusable - replaying from scratch with saved resolutions")
		}
		if err := j.setupEnv(true); err != nil {
			return err
		}
		if err := j.fresh(); err != nil {
			return err
		}
	}

	if err := j.gate(); err != nil {
		return err
	}
	if err := j.pickAll(); err != nil {
		return err
	}
	if err := j.verify(); err != nil {
		return err
	}
	bumps, err := j.dependencies()
	if err != nil {
		return err
	}
	if err := j.commands(); err != nil {
		return err
	}
	return j.finish(bumps)
}

// setupEnv (re)creates the working dir layout and git environment.
func (j *job) setupEnv(clean bool) error {
	if clean {
		if err := gitenv.RemoveAllWritable(j.workDir); err != nil {
			return fmt.Errorf("clear work dir: %w", err)
		}
	}
	gopathDir := filepath.Join(j.workDir, "gopath")
	j.repoDir = gitenv.RepoDirFor(gopathDir, j.p.CloneURL)
	if err := os.MkdirAll(j.repoDir, 0o700); err != nil {
		return fmt.Errorf("create repo dir: %w", err)
	}
	hosts := append([]string{gitenv.GitHost(j.p.CloneURL), gitenv.GitHost(j.pushURL), gitenv.GitHost(j.p.UpstreamURL)},
		godeps.ForkHosts(j.dirAutoReplaces)...)
	env, err := gitenv.BuildGitEnv(j.workDir, gopathDir, j.cfg, hosts)
	if err != nil {
		return err
	}
	j.r = runner.New(j.ctx, env, func(phase, message, log string) { j.emitRaw(phase, message, log) })
	j.rec.Attach(j.r)
	return nil
}

const targetLocalRef = "refs/upstream/target"

// fresh clones, locates the previous rebase, enumerates and records
// every commit, and creates the new `merge -s ours` base.
func (j *job) fresh() error {
	p := j.p
	r, repo := j.r, j.repoDir
	j.emit("cloning", "git clone "+p.CloneURL)
	if err := r.Run(repo, "git", "clone", "--branch", p.ForkBranch, p.CloneURL, "."); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	for _, kv := range [][2]string{
		{"user.name", j.cfg.GitUserName},
		{"user.email", j.cfg.GitUserEmail},
		{"rerere.enabled", strconv.FormatBool(p.Rerere != "off")},
		{"rerere.autoUpdate", "false"},
	} {
		if err := r.Run(repo, "git", "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	forkTip, err := gitrebase.RevParse(r, repo, "HEAD")
	if err != nil {
		return err
	}

	j.emit("fetching", "fetching upstream tags and branches from "+p.UpstreamURL)
	if err := r.Run(repo, "git", "remote", "add", "upstream", p.UpstreamURL); err != nil {
		return err
	}
	// Tags and branches both define "upstream content": a fork may rebase
	// onto a release tag or onto an untagged commit of upstream main.
	if err := r.Run(repo, "git", "fetch", "--no-tags", "upstream",
		"+refs/tags/*:"+gitrebase.UpstreamTagsNS+"*", "+refs/heads/*:"+gitrebase.UpstreamHeadsNS+"*"); err != nil {
		return fmt.Errorf("fetch upstream refs: %w", err)
	}
	// target_ref may name an upstream tag, an upstream branch, or
	// anything else upstream will serve (a SHA, a full ref).
	targetSHA, terr := gitrebase.RevParse(r, repo, gitrebase.UpstreamTagsNS+p.TargetRef)
	if terr != nil || targetSHA == "" {
		targetSHA, terr = gitrebase.RevParse(r, repo, gitrebase.UpstreamHeadsNS+p.TargetRef)
	}
	if terr != nil || targetSHA == "" {
		if err := r.Run(repo, "git", "fetch", "--no-tags", "upstream", "+"+p.TargetRef+":"+targetLocalRef); err != nil {
			return status.Errorf(codes.FailedPrecondition, "target_ref %q is not an upstream tag or branch and could not be fetched: %v", p.TargetRef, err)
		}
	} else if err := r.Run(repo, "git", "update-ref", targetLocalRef, targetSHA); err != nil {
		return err
	}
	j.targetRef = targetLocalRef
	targetSHA, err = gitrebase.RevParse(r, repo, targetLocalRef)
	if err != nil {
		return err
	}

	j.emit("enumerating", "locating the previous rebase")
	b, err := gitrebase.FindBoundary(r, repo, forkTip, p.PreviousRebaseCommit)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if code, _ := r.ExitCode(repo, "git", "merge-base", "--is-ancestor", targetSHA, b.UpstreamBase); code == 0 {
		return status.Errorf(codes.FailedPrecondition, "target %s (%s) is not newer than the previous rebase base %s (%s) - refusing a no-op / downgrade rebase",
			p.TargetRef, short(targetSHA), b.UpstreamBaseTag, short(b.UpstreamBase))
	}

	commits, err := gitrebase.Enumerate(r, repo, forkTip, b.Merge, targetLocalRef)
	if err != nil {
		return err
	}
	j.rows = make([]store.CommitRow, len(commits))
	for i, c := range commits {
		row := store.CommitRow{
			Ordinal: i + 1, OrigSHA: c.SHA, Author: c.Author, Subject: c.Subject,
			Kind: string(c.Kind), PR: c.PR, PatchID: c.PatchID, Detail: c.Detail, Status: stPending,
		}
		switch c.Kind {
		case gitrebase.KindDrop:
			row.Status = stDropped
		case gitrebase.KindQueuedPatch:
			row.Status = stRequeuedPatch
			row.Note = "re-applied from this step's patch queue (" + c.PatchID + ")"
		}
		j.rows[i] = row
	}
	r.Note(fmt.Sprintf("# previous rebase %s on %s; %d commit(s) to consider\n", short(b.Merge), b.UpstreamBaseTag, len(commits)))

	j.run = &store.Run{
		WorkflowRunID: j.req.GetWorkflowRunId(), StepID: j.req.GetStepId(), WorkflowID: j.req.GetWorkflowId(),
		InstanceID: j.req.GetInstanceId(), CloneURL: p.CloneURL, UpstreamURL: p.UpstreamURL, ForkBranch: p.ForkBranch,
		ForkTip: forkTip, TargetRef: p.TargetRef, TargetSHA: targetSHA, PrevMerge: b.Merge, PrevBase: b.UpstreamBase,
		PrevBaseTag: b.UpstreamBaseTag, Branch: p.BranchName, Phase: "enumerated", Status: "running", WorkDir: j.workDir,
	}
	if err := j.s.Store.ReplaceCommits(j.ctx, j.run.WorkflowRunID, j.run.StepID, j.rows); err != nil {
		return err
	}
	if err := j.saveRun(); err != nil {
		return err
	}

	j.emit("merging", fmt.Sprintf("git merge -s ours %s onto %s", p.ForkBranch, p.TargetRef))
	if err := r.Run(repo, "git", "checkout", "-q", "-B", p.BranchName, targetLocalRef); err != nil {
		return err
	}
	msg := fmt.Sprintf("Merge %s (%s) into %s with -s ours\n\nRebase of %s onto %s %s. The fork's UPSTREAM commits are re-applied on top of this merge.",
		p.ForkBranch, short(forkTip), p.BranchName, p.CloneURL, p.TargetRef, short(targetSHA))
	if err := r.Run(repo, "git", "merge", "-s", "ours", "--no-edit", "-m", msg, forkTip); err != nil {
		return err
	}
	if j.run.NewMerge, err = gitrebase.RevParse(r, repo, "HEAD"); err != nil {
		return err
	}

	if p.Rerere != "off" {
		entries, err := j.s.Store.ListRerere(j.ctx, j.run.WorkflowID, j.run.StepID)
		if err != nil {
			return err
		}
		conv := make([]gitrebase.RerereEntry, len(entries))
		for i, e := range entries {
			conv[i] = gitrebase.RerereEntry{ID: e.ID, Files: e.Files}
		}
		if err := gitrebase.ImportRerere(r, repo, conv); err != nil {
			return fmt.Errorf("restore rerere cache: %w", err)
		}
	}
	j.run.Phase = "merged"
	return j.saveRun()
}

// decisionKey indexes saved decisions.
func decisionKey(sha, q string) string { return sha + "/" + q }

func (j *job) decisions() (map[string]store.Decision, error) {
	list, err := j.s.Store.ListDecisions(j.ctx, j.run.WorkflowID, j.run.StepID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]store.Decision, len(list))
	for _, d := range list {
		m[decisionKey(d.OrigSHA, d.Question)] = d
	}
	return m, nil
}

// gate applies saved classification decisions and suspends (once, for
// all of them) if any commit still needs a human.
//
// Decisions are keyed by commit SHA. A re-picked commit gets a new SHA
// on every rebase, so a "carry" decision also matches later commits
// with the identical subject - but only "carry": a fallback may keep a
// commit, never lose one. A "drop" must be re-confirmed per SHA.
func (j *job) gate() error {
	dec, err := j.decisions()
	if err != nil {
		return err
	}
	carryBySubject := map[string]store.Decision{}
	for _, d := range dec {
		if d.Question == qClassify && d.Decision == "carry" && d.Subject != "" {
			carryBySubject[d.Subject] = d
		}
	}
	pending := 0
	for i := range j.rows {
		row := &j.rows[i]
		if terminal(row.Status) || !gitrebase.NeedsClassification(gitrebase.Kind(row.Kind)) {
			continue
		}
		d, ok := dec[decisionKey(row.OrigSHA, qClassify)]
		if !ok && gitrebase.Kind(row.Kind) != gitrebase.KindEvilMerge {
			if sd, found := carryBySubject[row.Subject]; found {
				d, ok = sd, true
				d.Reason = "same subject carried in an earlier run (" + short(sd.OrigSHA) + "): " + sd.Reason
			}
		}
		switch {
		case !ok:
			if row.Status != stAwaitingDecision {
				row.Status = stAwaitingDecision
				row.Note = "needs a decision: carry or drop?"
				if err := j.updateRow(row); err != nil {
					return err
				}
			}
			pending++
		case gitrebase.Kind(row.Kind) == gitrebase.KindEvilMerge:
			row.Status = stAcknowledged
			row.Note = "merge-only content acknowledged: " + d.Reason
			if err := j.updateRow(row); err != nil {
				return err
			}
		case d.Decision == "drop":
			row.Status = stDropped
			row.Note = "decided drop: " + d.Reason
			if err := j.updateRow(row); err != nil {
				return err
			}
		default: // carry: stays pending and gets picked
			if row.Status != stPending {
				row.Status = stPending
				row.Note = "decided carry: " + d.Reason
				if err := j.updateRow(row); err != nil {
					return err
				}
			}
		}
	}
	if pending > 0 {
		return j.suspend(suspendPrompt{Kind: "decide", Pending: pending},
			fmt.Sprintf("%d commit(s) need a carry/drop decision", pending))
	}
	return nil
}

func (j *job) shouldPick(row *store.CommitRow) bool {
	switch gitrebase.Kind(row.Kind) {
	case gitrebase.KindCarry, gitrebase.KindPR, gitrebase.KindUnclassified, gitrebase.KindMalformed:
		// Unclassified / malformed rows only reach here pending once a
		// "carry" decision was applied by gate().
		return true
	}
	return false
}

// pickAll walks the table in order, picking every non-terminal row.
func (j *job) pickAll() error {
	j.run.Phase = "picking"
	if err := j.saveRun(); err != nil {
		return err
	}
	dec, err := j.decisions()
	if err != nil {
		return err
	}
	for i := range j.rows {
		row := &j.rows[i]
		if terminal(row.Status) {
			continue
		}
		if !j.shouldPick(row) {
			return fmt.Errorf("internal: row %d (%s) is %s but not pickable", row.Ordinal, row.Kind, row.Status)
		}
		j.current = &currentCommit{Ordinal: row.Ordinal, Subject: row.Subject}

		var outcome gitrebase.Outcome
		if gitrebase.InProgress(j.r, j.repoDir) {
			// Resuming the pick we suspended on.
			if paths, _ := gitrebase.ConflictPaths(j.r, j.repoDir); len(paths) > 0 {
				outcome = gitrebase.OutcomeConflict
			} else {
				outcome = gitrebase.OutcomeEmpty
			}
		} else {
			j.emit("picking", fmt.Sprintf("[%d/%d] %s", row.Ordinal, len(j.rows), row.Subject))
			outcome, err = gitrebase.CherryPick(j.r, j.repoDir, row.OrigSHA)
			if err != nil {
				return err
			}
		}

		switch outcome {
		case gitrebase.OutcomePicked:
			if err := j.markCommitted(row, stPicked, "", "", ""); err != nil {
				return err
			}
		case gitrebase.OutcomeEmpty:
			if err := j.handleEmpty(row, dec); err != nil {
				return err
			}
		case gitrebase.OutcomeConflict:
			if err := j.handleConflict(row); err != nil {
				return err
			}
		}
	}
	j.current = nil
	return nil
}

func (j *job) markCommitted(row *store.CommitRow, st, resID, source, note string) error {
	head, err := gitrebase.RevParse(j.r, j.repoDir, "HEAD")
	if err != nil {
		return err
	}
	row.Status, row.NewSHA, row.ResolutionID, row.ResolutionSource = st, head, resID, source
	if note != "" {
		row.Note = note
	}
	return j.updateRow(row)
}

func (j *job) handleEmpty(row *store.CommitRow, dec map[string]store.Decision) error {
	if gitrebase.Kind(row.Kind) == gitrebase.KindPR {
		if err := gitrebase.Skip(j.r, j.repoDir); err != nil {
			return err
		}
		row.Status = stAlreadyUpstream
		row.Note = fmt.Sprintf("became empty - upstream PR #%d is already in %s", row.PR, j.p.TargetRef)
		return j.updateRow(row)
	}
	d, ok := dec[decisionKey(row.OrigSHA, qEmpty)]
	if !ok {
		// Refresh: the human may have just decided.
		fresh, err := j.decisions()
		if err != nil {
			return err
		}
		d, ok = fresh[decisionKey(row.OrigSHA, qEmpty)]
	}
	if !ok {
		row.Status = stAwaitingDecision
		row.Note = "became empty on " + j.p.TargetRef + " - drop it, or keep it as an empty commit?"
		if err := j.updateRow(row); err != nil {
			return err
		}
		return j.suspend(suspendPrompt{Kind: "decide", Pending: 1, Ordinal: row.Ordinal, Subject: row.Subject},
			fmt.Sprintf("commit %d became empty", row.Ordinal))
	}
	if d.Decision == "keep_empty" {
		if err := gitrebase.KeepEmpty(j.r, j.repoDir, row.OrigSHA); err != nil {
			return err
		}
		return j.markCommitted(row, stKeptEmpty, "", "", "became empty; kept: "+d.Reason)
	}
	if err := gitrebase.Skip(j.r, j.repoDir); err != nil {
		return err
	}
	row.Status = stDropped
	row.Note = "became empty; dropped: " + d.Reason
	return j.updateRow(row)
}

func (j *job) handleConflict(row *store.CommitRow) error {
	r, repo := j.r, j.repoDir
	fp, err := gitrebase.Fingerprint(r, repo)
	if err != nil {
		return err
	}

	// Which rr-cache entries rerere is tracking this conflict under, so
	// the entries it records can be linked to whatever resolves it.
	var rrIDs map[string]string
	if j.p.Rerere != "off" {
		if rrIDs, err = gitrebase.RerereIDs(r, repo); err != nil {
			return err
		}
	}

	// 1. Exact saved resolution - always auto-applied.
	res, err := j.s.Store.FindResolution(j.ctx, j.run.WorkflowID, j.run.StepID, fp)
	if err != nil {
		return err
	}
	if res != nil {
		source := "reused"
		if res.CreatedInRun == j.run.WorkflowRunID {
			source = "human"
		}
		j.emit("resolving", fmt.Sprintf("[%d] applying saved resolution %s", row.Ordinal, res.ResolutionID))
		if err := j.applySaved(row, res, source); err != nil {
			return fmt.Errorf("apply saved resolution %s to commit %d (%s): %w", res.ResolutionID, row.Ordinal, short(row.OrigSHA), err)
		}
		_ = j.s.Store.TouchResolution(j.ctx, res.ResolutionID)
		_ = j.s.Store.CloseConflicts(j.ctx, j.run.WorkflowRunID, j.run.StepID, fp)
		return j.exportRerere(rerereLink{ids: rrIDs, resolutionID: res.ResolutionID, subject: row.Subject})
	}

	// 2. rerere, when allowed to act on its own.
	if j.p.Rerere == "auto" {
		done, err := gitrebase.StageRerereResolved(r, repo)
		if err != nil {
			return err
		}
		if done {
			committed, err := gitrebase.Continue(r, repo)
			if err != nil {
				return err
			}
			if !committed {
				row.Status, row.ResolutionSource, row.Note = stSkipped, "rerere", "rerere resolution left no changes"
				return j.updateRow(row)
			}
			if err := j.markCommitted(row, stResolved, "", "rerere", "auto-resolved by git rerere"); err != nil {
				return err
			}
			return j.exportRerere(rerereLink{ids: rrIDs, subject: row.Subject})
		}
	}

	// 3. A human has to answer.
	files, err := gitrebase.CaptureConflict(r, repo, j.p.Rerere != "off")
	if err != nil {
		return err
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return err
	}
	conflictID, err := j.s.Store.OpenConflictID(j.ctx, j.run.WorkflowRunID, j.run.StepID)
	if err != nil {
		return err
	}
	if existing, _ := j.s.Store.GetConflict(j.ctx, conflictID); existing == nil || existing.Fingerprint != fp {
		conflictID = store.NewID("c_")
		if err := j.s.Store.InsertConflict(j.ctx, store.Conflict{
			ConflictID: conflictID, WorkflowRunID: j.run.WorkflowRunID, StepID: j.run.StepID, WorkflowID: j.run.WorkflowID,
			Ordinal: row.Ordinal, OrigSHA: row.OrigSHA, Subject: row.Subject, Fingerprint: fp, Files: filesJSON,
		}); err != nil {
			return err
		}
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	row.Status = stConflict
	row.Note = "conflict in " + strings.Join(paths, ", ")
	if err := j.updateRow(row); err != nil {
		return err
	}
	return j.suspend(suspendPrompt{Kind: "conflict", ConflictID: conflictID, Ordinal: row.Ordinal, Subject: row.Subject},
		fmt.Sprintf("commit %d conflicts", row.Ordinal))
}

func (j *job) applySaved(row *store.CommitRow, res *store.Resolution, source string) error {
	if res.Kind == "skip" {
		if err := gitrebase.Skip(j.r, j.repoDir); err != nil {
			return err
		}
		row.Status, row.ResolutionID, row.ResolutionSource = stSkipped, res.ResolutionID, source
		row.Note = "skipped by resolution: " + res.Reason
		return j.updateRow(row)
	}
	var files map[string]gitrebase.FileResolution
	if err := json.Unmarshal(res.Files, &files); err != nil {
		return fmt.Errorf("decode resolution files: %w", err)
	}
	if err := gitrebase.ApplyResolution(j.r, j.repoDir, files); err != nil {
		return err
	}
	committed, err := gitrebase.Continue(j.r, j.repoDir)
	if err != nil {
		return err
	}
	if !committed {
		row.Status, row.ResolutionID, row.ResolutionSource = stSkipped, res.ResolutionID, source
		row.Note = "resolution left no changes (commit is redundant on the new base)"
		return j.updateRow(row)
	}
	return j.markCommitted(row, stResolved, res.ResolutionID, source, "")
}

// rerereLink says what produced the rr-cache entries of the conflict
// just resolved: ids maps conflicted path → entry id (from MERGE_RR).
type rerereLink struct {
	ids          map[string]string
	resolutionID string
	subject      string
}

// exportRerere stores the working copy's rr-cache, linking entries
// named in link to the resolution / commit that produced them.
func (j *job) exportRerere(link rerereLink) error {
	if j.p.Rerere == "off" {
		return nil
	}
	entries, err := gitrebase.ExportRerere(j.r, j.repoDir)
	if err != nil {
		return err
	}
	pathByID := map[string]string{}
	for path, id := range link.ids {
		pathByID[id] = path
	}
	conv := make([]store.RerereEntry, len(entries))
	for i, e := range entries {
		conv[i] = store.RerereEntry{ID: e.ID, Files: e.Files}
		if path, ok := pathByID[e.ID]; ok {
			conv[i].ResolutionID, conv[i].Subject, conv[i].Path = link.resolutionID, link.subject, path
		}
	}
	return j.s.Store.UpsertRerere(j.ctx, j.run.WorkflowID, j.run.StepID, conv)
}

// syncRerere makes a resumed working copy's rr-cache match the store:
// entries deleted (or wiped) while the run was suspended must not be
// used, nor re-exported back into the store.
func (j *job) syncRerere() error {
	if j.p.Rerere == "off" {
		return nil
	}
	stored, err := j.s.Store.ListRerereMeta(j.ctx, j.run.WorkflowID, j.run.StepID)
	if err != nil {
		return err
	}
	keep := make(map[string]bool, len(stored))
	for _, e := range stored {
		keep[e.ID] = true
	}
	return gitrebase.PruneRerere(j.r, j.repoDir, keep)
}

// verify is the completeness check: nothing silently lost.
func (j *job) verify() error {
	j.emit("verifying", "checking every commit was accounted for")
	var problems []string

	// Every new commit on top of the rebase merge, in one rev-list.
	onBranchOut, err := j.r.Output(j.repoDir, "git", "rev-list", "--no-merges", j.run.NewMerge+"..HEAD")
	if err != nil {
		return err
	}
	onBranch := map[string]bool{}
	for _, sha := range strings.Fields(onBranchOut) {
		onBranch[sha] = true
	}
	committed := 0
	var origs, news []string
	for _, row := range j.rows {
		if !terminal(row.Status) {
			problems = append(problems, fmt.Sprintf("commit %d (%s) is still %s", row.Ordinal, short(row.OrigSHA), row.Status))
		}
		if row.NewSHA != "" {
			committed++
			if !onBranch[row.NewSHA] {
				problems = append(problems, fmt.Sprintf("commit %d's new SHA %s is not on the rebased branch", row.Ordinal, short(row.NewSHA)))
			}
			origs = append(origs, row.OrigSHA)
			news = append(news, row.NewSHA)
		}
	}
	if len(onBranch) != committed {
		problems = append(problems, fmt.Sprintf("branch has %d commit(s) on top of the rebase merge but the table accounts for %d", len(onBranch), committed))
	}
	if len(problems) > 0 {
		return status.Error(codes.FailedPrecondition, "completeness check failed:\n  "+strings.Join(problems, "\n  "))
	}

	// Review aid: which re-applied commits carry a different change than
	// the original (conflict resolutions, context the pick had to
	// absorb). We already know the original↔new pairing, so compare
	// patch-ids in two batched passes and range-diff only the changed
	// pairs one-to-one - a whole-range `git range-diff` pairs every old
	// commit against every new one, which never finishes on large
	// histories with generated-file commits.
	j.emit("verifying", fmt.Sprintf("comparing %d re-applied commit(s) with their originals", committed))
	oldIDs, err := gitrebase.PatchIDs(j.r, j.repoDir, origs)
	if err != nil {
		return err
	}
	newIDs, err := gitrebase.PatchIDs(j.r, j.repoDir, news)
	if err != nil {
		return err
	}
	var rd strings.Builder
	changed := 0
	for i := range j.rows {
		row := &j.rows[i]
		if row.NewSHA == "" || oldIDs[row.OrigSHA] == newIDs[row.NewSHA] {
			continue
		}
		changed++
		out, err := j.r.Output(j.repoDir, "git", "range-diff", "--no-color",
			row.OrigSHA+"^.."+row.OrigSHA, row.NewSHA+"^.."+row.NewSHA)
		if err != nil {
			out = "range-diff unavailable: " + err.Error()
		}
		fmt.Fprintf(&rd, "── #%d %s\n%s\n", row.Ordinal, row.Subject, out)
		if !strings.Contains(row.Note, "differs from original") {
			if row.Note != "" {
				row.Note += " · "
			}
			row.Note += "differs from original"
			if err := j.updateRow(row); err != nil {
				return err
			}
		}
	}
	if changed == 0 {
		rd.WriteString("Every re-applied commit carries the same change as its original.\n")
	}
	j.run.RangeDiff = rd.String()
	j.run.Phase = "verified"
	if err := j.exportRerere(rerereLink{}); err != nil {
		return err
	}
	return j.saveRun()
}

// dependencies runs the shared generic-go-dep pipeline per dir.
func (j *job) dependencies() ([]godeps.AppliedBump, error) {
	if len(j.p.Dirs) == 0 {
		return nil, nil
	}
	j.run.Phase = "dependencies"
	if err := j.saveRun(); err != nil {
		return nil, err
	}
	patchesByDir, err := patchqueue.LoadByDir(j.ctx, j.s.Store.Patches, j.req)
	if err != nil {
		return nil, err
	}
	hooks := godeps.Hooks{
		Messages: godeps.Messages{Deps: depsMessage, Vendor: vendorMessage, Make: makeMessage},
		AfterVendor: func(d godeps.Dir, dirCwd string) error {
			patches := patchesByDir[d.Path]
			if len(patches) == 0 {
				return nil
			}
			j.emit("patching", fmt.Sprintf("[%s] applying %d attached patch(es)", d.Path, len(patches)))
			for _, pt := range patches {
				if err := patchqueue.ApplyPatch(j.r, j.repoDir, dirCwd, d.Path, pt, patchqueue.ApplyOptions{
					Message: patchMessage,
					Trailer: gitrebase.PatchTrailer + ": " + pt.PatchID,
				}); err != nil {
					return fmt.Errorf("[%s] apply patch %q: %w", d.Path, pt.Name, err)
				}
			}
			return nil
		},
	}
	var all []godeps.AppliedBump
	for di, d := range j.p.Dirs {
		bumps, err := godeps.ProcessDir(j.r, j.repoDir, d, j.dirAutoReplaces[di], hooks)
		if err != nil {
			return nil, err
		}
		all = append(all, bumps...)
	}
	return all, nil
}

// commands runs the step's bare shell commands, last of everything that
// commits. A command that leaves the tree unmodified gets no commit.
func (j *job) commands() error {
	if len(j.p.Commands) == 0 {
		return nil
	}
	j.run.Phase = "commands"
	if err := j.saveRun(); err != nil {
		return err
	}
	for i, c := range j.p.Commands {
		cwd := filepath.Join(j.repoDir, c.Dir)
		j.emit("commands", fmt.Sprintf("[%d/%d] %s%s", i+1, len(j.p.Commands), scope(c.Dir), c.Name))
		if err := j.r.Run(cwd, "sh", "-c", c.Run); err != nil {
			return fmt.Errorf("commands[%d] %q: %w", i, c.Name, err)
		}
		if err := j.r.Run(j.repoDir, "git", "add", "-A"); err != nil {
			return fmt.Errorf("commands[%d] %q: git add: %w", i, c.Name, err)
		}
		hasChanges, err := j.r.HasStagedChanges(j.repoDir)
		if err != nil {
			return fmt.Errorf("commands[%d] %q: check staged changes: %w", i, c.Name, err)
		}
		if !hasChanges {
			j.r.Note(fmt.Sprintf("# command %q produced no changes - no commit\n", c.Name))
			continue
		}
		if err := j.r.Run(j.repoDir, "git", "commit", "-m", commandMessage(c)); err != nil {
			return fmt.Errorf("commands[%d] %q: git commit: %w", i, c.Name, err)
		}
	}
	return nil
}

func (j *job) finish(bumps []godeps.AppliedBump) error {
	j.emit("pushing", "git push --force "+j.pushURL+" "+j.p.BranchName)
	if err := j.r.Run(j.repoDir, "git", "push", "--force", j.pushURL, j.p.BranchName); err != nil {
		return fmt.Errorf("git push %s %s: %w", j.pushURL, j.p.BranchName, err)
	}
	head, err := gitrebase.RevParse(j.r, j.repoDir, "HEAD")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	j.run.PushedSHA, j.run.Phase, j.run.Status, j.run.FinishedAt = head, "done", "succeeded", &now
	if err := j.saveRun(); err != nil {
		return err
	}
	j.emit("done", "pushed "+j.p.BranchName+" to "+j.pushURL)
	if err := j.s.removeWorkDir(j.workDir); err != nil {
		j.emit("cleanup", fmt.Sprintf("failed to fully remove work dir %s: %v", j.workDir, err))
	}

	type commitOut struct {
		OrigSHA string `json:"orig_sha"`
		NewSHA  string `json:"new_sha,omitempty"`
		Subject string `json:"subject"`
		Kind    string `json:"kind"`
		PR      int    `json:"pr,omitempty"`
		Status  string `json:"status"`
	}
	commits := make([]commitOut, len(j.rows))
	for i, row := range j.rows {
		commits[i] = commitOut{row.OrigSHA, row.NewSHA, row.Subject, row.Kind, row.PR, row.Status}
	}
	if bumps == nil {
		bumps = []godeps.AppliedBump{}
	}
	outs := map[string]any{
		"ref": j.p.BranchName, "commit": head, "repo": j.p.CloneURL, "fork_url": j.pushURL,
		"target_ref": j.p.TargetRef, "target_commit": j.run.TargetSHA, "previous_rebase_commit": j.run.PrevMerge,
		"bumps": bumps, "commits": commits,
	}
	values := make(map[string][]byte, len(outs))
	for k, v := range outs {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		values[k] = b
	}
	result, _ := json.Marshal(map[string]any{"outputs": outs, "counts": j.counts()})
	return j.stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Final{Final: &pluginpb.ExecuteFinal{
			Result:  result,
			Outputs: &pluginpb.StepOutputs{Values: values},
		}},
	})
}

// suspend checkpoints and ends the stream with ExecuteSuspend.
func (j *job) suspend(prompt suspendPrompt, message string) error {
	head, err := gitrebase.RevParse(j.r, j.repoDir, "HEAD")
	if err != nil {
		return err
	}
	j.run.CheckpointHead = head
	j.run.Status = "suspended"
	j.run.Phase = "awaiting_" + prompt.Kind
	if err := j.exportRerere(rerereLink{}); err != nil {
		return err
	}
	if err := j.saveRun(); err != nil {
		return err
	}
	j.emit("awaiting", message)
	if err := execstream.SendSuspendJSON(j.stream, prompt); err != nil {
		return err
	}
	return errSuspended
}

// fail records the error and drops the working copy (a failed run is
// never resumed; a retry replays from scratch). The working copy goes
// even when no run row was written yet - a clone that fails before
// enumeration (bad previous_rebase_commit, downgrade target, …) is
// just as dead.
func (j *job) fail(cause error) {
	defer func() { _ = j.s.removeWorkDir(j.workDir) }()
	if j.run == nil {
		return
	}
	now := time.Now().UTC()
	j.run.Status, j.run.Error, j.run.FinishedAt = "failed", cause.Error(), &now
	// Use a fresh context: the stream's may already be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	j.run.UpdatedAt = now
	_ = j.s.Store.SaveRun(ctx, j.run)
}

func (j *job) saveRun() error {
	return j.s.Store.SaveRun(j.ctx, j.run)
}

func (j *job) updateRow(row *store.CommitRow) error {
	if err := j.s.Store.UpdateCommit(j.ctx, j.run.WorkflowRunID, j.run.StepID, *row); err != nil {
		return err
	}
	j.run.TableRev = time.Now().UnixNano()
	if err := j.saveRun(); err != nil {
		return err
	}
	j.emit(j.phase, "")
	return nil
}

func (j *job) counts() map[string]int {
	m := map[string]int{}
	for _, row := range j.rows {
		m[row.Status]++
	}
	return m
}

// emit publishes a progress event with a new phase / message.
func (j *job) emit(phase, message string) {
	if phase != "" {
		j.phase = phase
	}
	tail := ""
	if j.r != nil {
		tail = j.r.Tail()
	}
	j.emitRaw(j.phase, message, tail)
}

func (j *job) emitRaw(phase, message, log string) {
	if j.stream == nil {
		return
	}
	if phase == "running" {
		// Runner command events keep the job's own phase label.
		phase = j.phase
	}
	pp := progressPayload{Phase: phase, Message: message, Log: log, Total: len(j.rows), Current: j.current}
	if len(j.rows) > 0 {
		pp.Counts = j.counts()
	}
	if j.run != nil {
		pp.TableRev = j.run.TableRev
	}
	payload, err := json.Marshal(pp)
	if err != nil {
		return
	}
	_ = j.stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Progress{Progress: &pluginpb.ExecuteProgress{Payload: payload}},
	})
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ── generated commit messages ─────────────────────────────────────────────────
// Everything this plugin generates is UPSTREAM: <drop>: so the next
// rebase drops it and regenerates it, never double-applies it.

const dropPrefix = "UPSTREAM: <drop>: "

func scope(path string) string {
	if path == "" || path == "." {
		return ""
	}
	return "(" + path + ") "
}

func depsMessage(path string, bumps []godeps.AppliedBump) string {
	var b strings.Builder
	if len(bumps) == 1 {
		fmt.Fprintf(&b, "%s%supdate %s to %s\n\n", dropPrefix, scope(path), bumps[0].Dependency, bumps[0].TargetVersion)
	} else {
		fmt.Fprintf(&b, "%s%sbump %d dependencies\n\n", dropPrefix, scope(path), len(bumps))
	}
	b.WriteString(godeps.BumpLines(bumps))
	if godeps.AnyHasReplace(bumps) {
		b.WriteString("\nThe replace directives above point at fork commits that the\n")
		b.WriteString("canonical module paths can't yet resolve.\n")
	}
	b.WriteString("\n[generated by upstream-rebase plugin]")
	return b.String()
}

func vendorMessage(path string) string {
	return dropPrefix + scope(path) + "go mod vendor\n\n[generated by upstream-rebase plugin]"
}

func makeMessage(path, target string) string {
	return fmt.Sprintf("%s%smake %s\n\n[generated by upstream-rebase plugin]", dropPrefix, scope(path), target)
}

func commandMessage(c shellCommand) string {
	return fmt.Sprintf("%s%s%s\n\n$ %s\n\n[generated by upstream-rebase plugin]", dropPrefix, scope(c.Dir), c.Name, c.Run)
}

func patchMessage(dirPath string, pt patchqueue.Patch) string {
	return fmt.Sprintf("%s%sapply patch %q\n\n[attached patch %s, applied by upstream-rebase plugin]",
		dropPrefix, scope(dirPath), pt.Name, pt.PatchID)
}
