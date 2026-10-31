// Package service implements the generic-go-dep plugin's gRPC contract.
//
// Model:
//   - Clone the upstream repo (clone_url, read-only for us) once.
//   - Branch, then for each entry in `dirs` (one per Go module inside
//     the repo): bump its deps, go mod tidy, optionally go mod vendor,
//     apply the step's attached patches, run its make targets, commit
//     each stage. Multi-module repos with independent go.mod files are
//     the reason `dirs` exists - a monorepo with e.g.
//     `staging/src/k8s.io/api` and `staging/src/k8s.io/client-go` can
//     bump each module separately in one branch / push.
//   - Run the step's bare shell `commands` in order, each one's changes
//     committed on its own.
//   - Push the new branch to a *fork* (push_url) using a configured
//     SSH key. We never push back to the clone source - the assumption
//     is that a separate review/PR step (not in this plugin) takes the
//     pushed branch and merges it into the upstream.
//
// The mechanics (git/SSH isolation, __ref resolution, the per-dir
// bump/tidy/vendor/patch/make pipeline, the patch queue, shell
// commands) live in
// pluginkit so the upstream-rebase plugin runs exactly the same code.
// What stays here is this plugin's manifest, params shape, commit
// message wording, and progress payload.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobsee/upstream-readiness/plugin-generic-go-dep/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/cmdlog"
	"github.com/jacobsee/upstream-readiness/pluginkit/gitenv"
	"github.com/jacobsee/upstream-readiness/pluginkit/godeps"
	"github.com/jacobsee/upstream-readiness/pluginkit/mfeassets"
	"github.com/jacobsee/upstream-readiness/pluginkit/patchqueue"
	"github.com/jacobsee/upstream-readiness/pluginkit/refs"
	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
	"github.com/jacobsee/upstream-readiness/pluginkit/shellcmds"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GoDepUpdater carries the plugin's shared handles. Store owns the
// per-(workflow_id, step_id) patch queue the workflow-step-config MFE
// reads and writes, and handleUpdate consumes on Execute.
type GoDepUpdater struct {
	pluginpb.UnimplementedPluginServer
	Assets *mfeassets.Server
	Store  *store.Store
	// WorkRoot holds one scratch dir per in-flight `update` (clone +
	// GOPATH). Nothing in it outlives the Execute call that created it:
	// runUpdate removes its own dir on every return path, and main
	// empties the root at startup to catch runs killed mid-flight. The
	// plugin never suspends, so it has nothing for ReleaseStep to drop
	// and leaves that RPC unimplemented.
	WorkRoot string
}

// SweepWorkRoot empties WorkRoot. Only safe before the gRPC server
// starts, when no update can be running; everything left is an orphan
// from a process that died mid-run. Returns the paths it removed.
func (g *GoDepUpdater) SweepWorkRoot() ([]string, error) {
	entries, err := os.ReadDir(g.WorkRoot)
	if err != nil {
		return nil, err
	}
	var removed, errs []string
	for _, e := range entries {
		p := filepath.Join(g.WorkRoot, e.Name())
		if err := gitenv.RemoveAllWritable(p); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		removed = append(removed, p)
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("sweep %s: %s", g.WorkRoot, strings.Join(errs, "; "))
	}
	return removed, nil
}

// PurgeWorkflow deletes everything kept for a workflow the orchestrator
// is purging: its steps' patch queues and its runs' command history.
// Nothing on disk outlives an Execute call, so there are no working
// copies to remove.
func (g *GoDepUpdater) PurgeWorkflow(ctx context.Context, req *pluginpb.PurgeWorkflowRequest) (*pluginpb.PurgeWorkflowResponse, error) {
	wf := req.GetWorkflowId()
	if wf == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow_id is required")
	}
	if g.Store == nil {
		return nil, status.Error(codes.FailedPrecondition, "plugin store is not configured; set --pg-dsn or PG_DSN")
	}
	patches, err := g.Store.Patches.DeleteWorkflow(ctx, wf)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete patch queue: %v", err)
	}
	commands, err := g.Store.Commands.DeleteWorkflow(ctx, wf)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete command history: %v", err)
	}
	return &pluginpb.PurgeWorkflowResponse{Deleted: map[string]int64{
		"patches": patches, "command_log": commands,
	}}, nil
}

func (g *GoDepUpdater) GetAsset(req *pluginpb.GetAssetRequest, stream pluginpb.Plugin_GetAssetServer) error {
	return g.Assets.GetAsset(req, stream)
}

func (g *GoDepUpdater) Describe(_ context.Context, _ *pluginpb.DescribeRequest) (*pluginpb.PluginManifest, error) {
	actions := []*pluginpb.ActionDescriptor{
		{
			Name:        "update",
			Description: "Bump Go module dependencies in the cloned repo, run make targets, then shell commands, and push the branch to a fork.",
			Outputs: []*pluginpb.ActionOutput{
				{Name: "ref", Description: "Branch name pushed to the fork."},
				{Name: "commit", Description: "Full SHA of the commit pushed to the fork."},
				{Name: "repo", Description: "Upstream repository URL the change was based on."},
				{Name: "fork_url", Description: "Fork URL the branch was pushed to."},
				{Name: "bumps", Description: "Applied bump records: list of {path, dependency, target_version, replace?} - path is the module dir the bump was applied in."},
			},
		},
	}
	// The patches_* actions back the workflow-step-config MFE's patch
	// queue (pluginkit/patchqueue).
	actions = append(actions, patchqueue.ActionDescriptors()...)
	// cmdlog_* back the step-runtime / plugin-home command history.
	actions = append(actions, cmdlog.ActionDescriptors()...)
	return &pluginpb.PluginManifest{
		Id:          "generic-go-dep",
		DisplayName: "Go Dependency Updater",
		Version:     "0.5.0",
		MfeEntry:    g.Assets.Entry(),
		MfeElement:  "",
		MfeSlots: []*pluginpb.MfeSlot{
			{Slot: "action"},
			{Slot: "instance-config"},
			{Slot: "step-runtime"},
			{Slot: "workflow-step-config"},
			{Slot: "instance-list-item"},
			{Slot: "sidebar-badge"},
			{Slot: "plugin-home"},
		},
		Actions: actions,
	}, nil
}

// stepParams is the shape the plugin's per-action UI produces. Opaque
// to the orchestrator. Any leaf value may be an {"__ref": {...}} marker.
//
// A step operates on one cloned repo but may bump modules in multiple
// directories inside it - that's what `Dirs` enumerates. Each dir
// entry carries its own updates, make targets, and vendor decision so
// a monorepo with independent go.mod files can be handled in one push.
type stepParams struct {
	CloneURL string `json:"clone_url"`
	// PushURL is the fork the new branch is pushed to. If empty, the
	// plugin derives it from the instance's fork_url_template by
	// substituting {repo} with the upstream repo's basename.
	PushURL    string       `json:"push_url"`
	BranchName string       `json:"branch_name"`
	Dirs       []godeps.Dir `json:"dirs"`
	// Commands are bare shell commands run in order after every dir's
	// pipeline (bumps, vendor, patches, make targets). Each one's
	// changes land in their own commit (pluginkit/shellcmds). Optional.
	Commands []shellcmds.Command `json:"commands"`
}

type updateResult struct {
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Repo    string `json:"repo"`
	ForkURL string `json:"fork_url"`
	Output  string `json:"output"`
	// Bumps records what actually happened, in the same order as
	// stepParams.Updates. Downstream tooling (a future PR-opening
	// plugin, changelog generator, etc.) uses this to describe the
	// change without having to parse commit messages.
	Bumps []godeps.AppliedBump `json:"bumps"`
}

func (g *GoDepUpdater) Execute(req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	if req.GetAction() == "update" {
		return g.handleUpdate(stream.Context(), req, stream)
	}
	var patches *patchqueue.Store
	var commands *cmdlog.Store
	if g.Store != nil {
		patches, commands = g.Store.Patches, g.Store.Commands
	}
	if handled, err := patchqueue.Handle(stream.Context(), patches, req, stream); handled {
		return err
	}
	if handled, err := cmdlog.Handle(stream.Context(), commands, req, stream); handled {
		return err
	}
	return fmt.Errorf("unknown action %q", req.GetAction())
}

// progressPayload is the shape this plugin emits on the Execute stream.
// Opaque to the orchestrator; consumed by the plugin's own MFE mounted
// in the "step-runtime" slot.
type progressPayload struct {
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
	// Log is the accumulated command output so far. Overwritten (not
	// appended) on every progress event so the browser always renders
	// the latest snapshot without having to reconcile deltas.
	Log string `json:"log,omitempty"`
}

// emit sends a Progress event on the Execute stream. Failures are
// silently dropped - losing a progress event is far less bad than
// crashing the actual work.
func emit(stream pluginpb.Plugin_ExecuteServer, phase, message, log string) {
	if stream == nil {
		return
	}
	payload, err := json.Marshal(progressPayload{Phase: phase, Message: message, Log: log})
	if err != nil {
		return
	}
	_ = stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Progress{Progress: &pluginpb.ExecuteProgress{Payload: payload}},
	})
}

func (g *GoDepUpdater) Validate(_ context.Context, req *pluginpb.ValidateRequest) (*pluginpb.ValidateResponse, error) {
	switch req.GetKind() {
	case "instance":
		return validateInstance(req.GetPayload()), nil
	case "step":
		return validateStep(req.GetPayload()), nil
	default:
		return &pluginpb.ValidateResponse{
			Valid: false, Errors: []string{fmt.Sprintf("unknown validate kind %q", req.GetKind())},
		}, nil
	}
}

func validateInstance(payload []byte) *pluginpb.ValidateResponse {
	var cfg gitenv.InstanceConfig
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &cfg); err != nil {
			return &pluginpb.ValidateResponse{Valid: false, Errors: []string{"invalid JSON: " + err.Error()}}
		}
	}
	errs := gitenv.ValidateInstance(cfg)
	return &pluginpb.ValidateResponse{Valid: len(errs) == 0, Errors: errs}
}

func validateStep(payload []byte) *pluginpb.ValidateResponse {
	// Step params may contain unresolved __ref markers at edit time, so
	// only check for key presence, not value types.
	var raw map[string]any
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &raw); err != nil {
			return &pluginpb.ValidateResponse{Valid: false, Errors: []string{"invalid JSON: " + err.Error()}}
		}
	}
	var errs []string
	if _, ok := raw["clone_url"]; !ok {
		errs = append(errs, "clone_url is required")
	}
	dirsRaw, present := raw["dirs"]
	errs = append(errs, godeps.ValidateRaw(dirsRaw, present, godeps.Options{})...)
	if cmdsRaw, present := raw["commands"]; present {
		errs = append(errs, shellcmds.ValidateRaw(cmdsRaw)...)
	}
	return &pluginpb.ValidateResponse{Valid: len(errs) == 0, Errors: errs}
}

func (g *GoDepUpdater) handleUpdate(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	emit(stream, "preparing", "decoding instance and step params", "")

	var cfg gitenv.InstanceConfig
	if blob := req.GetInstanceConfig(); len(blob) > 0 {
		if err := json.Unmarshal(blob, &cfg); err != nil {
			return fmt.Errorf("decode instance config: %w", err)
		}
	}
	if err := gitenv.CheckForExecute(req.GetInstanceId(), cfg); err != nil {
		return err
	}

	// Peek at the raw params BEFORE resolving __ref markers so we can
	// see, per update, whether target_version came from an upstream
	// step's output - which tells us whether we need a `replace`
	// directive so `go mod tidy` can find the referenced commit on the
	// upstream's fork.
	var rawParams map[string]any
	if raw := req.GetPayload(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &rawParams); err != nil {
			return fmt.Errorf("decode step params: %w", err)
		}
	}

	resolved, err := refs.ResolveRefs(req.GetPayload(), req.GetUpstreamOutputs())
	if err != nil {
		return fmt.Errorf("resolve upstream references: %w", err)
	}
	var p stepParams
	if err := json.Unmarshal(resolved, &p); err != nil {
		return fmt.Errorf("decode step params: %w", err)
	}
	if p.CloneURL == "" {
		return fmt.Errorf("clone_url is required")
	}
	if err := godeps.Normalize(p.Dirs, cfg.DefaultMakeTargets, godeps.Options{}); err != nil {
		return err
	}
	if err := shellcmds.Normalize(p.Commands); err != nil {
		return err
	}

	pushURL, err := gitenv.ResolvePushURL(p.PushURL, p.CloneURL, cfg.ForkURLTemplate)
	if err != nil {
		return err
	}
	if pushURL == p.CloneURL {
		return fmt.Errorf("refusing to push to the clone source %q - set a fork URL via push_url or instance fork_url_template", p.CloneURL)
	}

	dirAutoReplaces, err := godeps.DetectAutoReplaces(rawParams, p.Dirs, req.GetUpstreamOutputs())
	if err != nil {
		return err
	}

	// Fetch this step's attached patches, grouped by target dir. Skipped
	// for ad-hoc runs (no workflow context) - a patch queue keyed by
	// (workflow_id, step_id) doesn't apply outside a workflow, so ad-hoc
	// invocations apply no patches. Any DB error here fails the run loudly rather
	// than silently dropping patches the workflow author expected to
	// land.
	var patchStore *patchqueue.Store
	if g.Store != nil {
		patchStore = g.Store.Patches
	}
	if patchqueue.InWorkflow(req) && patchStore != nil {
		emit(stream, "preparing", "loading attached patches", "")
	}
	patchesByDir, err := patchqueue.LoadByDir(ctx, patchStore, req)
	if err != nil {
		return err
	}

	var rec *cmdlog.Recorder
	if g.Store != nil {
		rec = cmdlog.NewRecorder(ctx, g.Store.Commands, req)
	}
	result, err := runUpdate(ctx, g.WorkRoot, rec, cfg, p, pushURL, dirAutoReplaces, patchesByDir, stream)
	if err != nil {
		return err
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	refBytes, _ := json.Marshal(result.Ref)
	commitBytes, _ := json.Marshal(result.Commit)
	repoBytes, _ := json.Marshal(result.Repo)
	forkBytes, _ := json.Marshal(result.ForkURL)
	bumpsBytes, _ := json.Marshal(result.Bumps)
	return stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Final{Final: &pluginpb.ExecuteFinal{
			Result: resultJSON,
			Outputs: &pluginpb.StepOutputs{
				Values: map[string][]byte{
					"ref":      refBytes,
					"commit":   commitBytes,
					"repo":     repoBytes,
					"fork_url": forkBytes,
					"bumps":    bumpsBytes,
				},
			},
		}},
	})
}

func runUpdate(ctx context.Context, workRoot string, rec *cmdlog.Recorder, cfg gitenv.InstanceConfig, p stepParams, pushURL string, dirAutoReplaces [][]*refs.AutoReplaceDirective, patchesByDir map[string][]patchqueue.Patch, stream pluginpb.Plugin_ExecuteServer) (*updateResult, error) {
	dir, err := os.MkdirTemp(workRoot, "update-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if err := gitenv.RemoveAllWritable(dir); err != nil {
			emit(stream, "cleanup", fmt.Sprintf("failed to fully remove temp dir %s: %v", dir, err), "")
		}
	}()

	// GOPATH lives under the per-run dir too (never the host's), so it's
	// swept up by the same cleanup. We clone into
	// $GOPATH/src/<module-path-of-clone-url> (see gitenv.RepoDirFor).
	gopathDir := filepath.Join(dir, "gopath")
	repoDir := gitenv.RepoDirFor(gopathDir, p.CloneURL)
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		return nil, fmt.Errorf("create repo dir: %w", err)
	}

	// Collect every hostname we might need SSH auth to reach so
	// BuildGitEnv can install insteadOf rewrites for each. The fork
	// hosts may differ from the clone host when upstream steps pushed
	// to forks on a different origin, and different updates may point
	// at different forks.
	hosts := append([]string{gitenv.GitHost(p.CloneURL), gitenv.GitHost(pushURL)}, godeps.ForkHosts(dirAutoReplaces)...)
	env, err := gitenv.BuildGitEnv(dir, gopathDir, cfg, hosts)
	if err != nil {
		return nil, err
	}

	r := runner.New(ctx, env, func(phase, message, log string) { emit(stream, phase, message, log) })
	rec.Attach(r)

	emit(stream, "cloning", "git clone "+p.CloneURL, r.Tail())
	if err := r.Run(repoDir, "git", "clone", p.CloneURL, "."); err != nil {
		return nil, fmt.Errorf("git clone: %w", err)
	}
	_ = r.Run(repoDir, "git", "config", "user.name", cfg.GitUserName)
	_ = r.Run(repoDir, "git", "config", "user.email", cfg.GitUserEmail)

	branch := p.BranchName
	if branch == "" {
		branch = godeps.DeriveBranchName(p.Dirs)
	}
	if err := r.Run(repoDir, "git", "checkout", "-b", branch); err != nil {
		return nil, fmt.Errorf("git checkout -b %s: %w", branch, err)
	}

	hooks := godeps.Hooks{
		Messages: godeps.Messages{
			Deps:   commitMessage,
			Vendor: vendorCommitMessage,
			Make:   makeCommitMessage,
		},
		// Apply any patches the workflow author attached to (workflow_id,
		// step_id, dir) via the workflow-step-config MFE. Patches land
		// AFTER vendor and BEFORE make so make targets exercise the
		// patched code - the whole point of the patch queue is usually to
		// unbreak a build that a mechanical dep bump would otherwise
		// leave in a red state.
		AfterVendor: func(d godeps.Dir, dirCwd string) error {
			patches := patchesByDir[d.Path]
			if len(patches) == 0 {
				return nil
			}
			pathLabel := d.Path
			if pathLabel == "." {
				pathLabel = "(root)"
			}
			emit(stream, "patching", fmt.Sprintf("[%s] applying %d attached patch(es)", pathLabel, len(patches)), r.Tail())
			for _, pt := range patches {
				if err := patchqueue.ApplyPatch(r, repoDir, dirCwd, d.Path, pt, patchqueue.ApplyOptions{Message: patchCommitMessage}); err != nil {
					return fmt.Errorf("[%s] apply patch %q: %w", d.Path, pt.Name, err)
				}
			}
			return nil
		},
	}

	var allBumps []godeps.AppliedBump
	for di, d := range p.Dirs {
		dirBumps, err := godeps.ProcessDir(r, repoDir, d, dirAutoReplaces[di], hooks)
		if err != nil {
			return nil, err
		}
		allBumps = append(allBumps, dirBumps...)
	}

	// Shell commands commit last, after every dir's make targets.
	if err := shellcmds.Run(r, repoDir, p.Commands, shellCommitMessage); err != nil {
		return nil, err
	}

	emit(stream, "pushing", "git push --force "+pushURL+" "+branch, r.Tail())
	// Force push so Temporal activity retries (or re-runs of the same
	// workflow against the same branch name) overwrite stale fork state
	// rather than failing on non-fast-forward.
	if err := r.Run(repoDir, "git", "push", "--force", pushURL, branch); err != nil {
		return nil, fmt.Errorf("git push %s %s: %w", pushURL, branch, err)
	}
	emit(stream, "done", "pushed "+branch+" to "+pushURL, r.Tail())

	shaOut, _ := r.Output(repoDir, "git", "rev-parse", "HEAD")
	sha := strings.TrimSpace(shaOut)

	return &updateResult{
		Ref:     branch,
		Commit:  sha,
		Repo:    p.CloneURL,
		ForkURL: pushURL,
		Output:  r.Log(),
		Bumps:   allBumps,
	}, nil
}

// commitMessage produces the deps commit message for a single dir.
// Bumps whose target commit lives on a fork include a prominent note
// about the temporary replace directive so reviewers know to drop it
// once the upstream module absorbs the fork commit.
func commitMessage(path string, bumps []godeps.AppliedBump) string {
	prefix := commitPrefix(path)
	if len(bumps) == 1 {
		return singleBumpMessage(prefix, bumps[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sbump %d dependencies\n\n", prefix, len(bumps))
	b.WriteString(godeps.BumpLines(bumps))
	if godeps.AnyHasReplace(bumps) {
		b.WriteString("\nThe replace directives above point at fork commits that the\n")
		b.WriteString("canonical module paths can't yet resolve. Drop each replace\n")
		b.WriteString("once its corresponding upstream PR has merged and been tagged.\n")
	}
	b.WriteString("\n[generated by generic-go-dep plugin]")
	return b.String()
}

func singleBumpMessage(prefix string, bump godeps.AppliedBump) string {
	if bump.Replace == nil {
		return fmt.Sprintf("%supdate %s to %s\n\n[generated by generic-go-dep plugin]", prefix, bump.Dependency, bump.TargetVersion)
	}
	return fmt.Sprintf(
		"%supdate %s to %s\n\n"+
			"Includes a temporary replace directive:\n"+
			"    %s => %s %s\n"+
			"pointing at commit %s produced by upstream workflow step %q.\n"+
			"Remove the replace once the upstream module has been merged\n"+
			"and re-tagged so the canonical module path resolves the SHA.\n\n"+
			"[generated by generic-go-dep plugin]",
		prefix, bump.Dependency, bump.TargetVersion,
		bump.Dependency, bump.Replace.ForkModulePath, bump.Replace.ForkCommit,
		bump.Replace.ForkCommit, bump.Replace.UpstreamStepID,
	)
}

func vendorCommitMessage(path string) string {
	return commitPrefix(path) + "go mod vendor\n\n[generated by generic-go-dep plugin]"
}

func makeCommitMessage(path, target string) string {
	return fmt.Sprintf("%smake %s\n\n[generated by generic-go-dep plugin]", commitPrefix(path), target)
}

func shellCommitMessage(c shellcmds.Command) string {
	return fmt.Sprintf("%s%s\n\n$ %s\n\n[generated by generic-go-dep plugin]", commitPrefix(c.Dir), c.Name, c.Run)
}

// commitPrefix scopes each commit's subject line to the module dir it
// changed. Root-module commits keep the plain `chore:` prefix; nested
// modules use `chore(<path>):` so `git log --oneline` makes it obvious
// which module a commit belongs to in a multi-module repo.
func commitPrefix(path string) string {
	if path == "" || path == "." {
		return "chore: "
	}
	return fmt.Sprintf("chore(%s): ", path)
}

func patchCommitMessage(dirPath string, pt patchqueue.Patch) string {
	return fmt.Sprintf("%sapply patch %q\n\n[attached patch %s, applied by generic-go-dep plugin]",
		commitPrefix(dirPath), pt.Name, pt.PatchID)
}
