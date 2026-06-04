package patchqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobsee/upstream-readiness/pluginkit/execstream"
	"github.com/jacobsee/upstream-readiness/pluginkit/runner"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// ActionDescriptors are the manifest entries for the patches_* actions.
// They back the workflow-step-config MFE's patch queue and aren't meant
// to appear in a workflow YAML - the MFE calls them ad-hoc via
// pluginApi.execute with workflow_id + step_id scoping.
func ActionDescriptors() []*pluginpb.ActionDescriptor {
	return []*pluginpb.ActionDescriptor{
		{Name: "patches_list", Description: "List patches attached to a (workflow_id, step_id)."},
		{Name: "patches_upsert", Description: "Create or update a patch for a (workflow_id, step_id)."},
		{Name: "patches_delete", Description: "Delete a patch from a (workflow_id, step_id)."},
		{Name: "patches_reorder", Description: "Reassign patch ordinals for a (workflow_id, step_id)."},
	}
}

// Handle serves the patches_* actions. handled is false for any other
// action so the plugin's own router can carry on.
//
// All four handlers key their state by (workflow_id, step_id) taken
// from the ExecuteRequest identity tuple. The MFE never sends those
// values in its payload - pluginApi.execute lifts them into the request
// envelope on our behalf. Ad-hoc calls (no workflow context) are
// rejected because a patch queue outside a specific step is
// meaningless.
func Handle(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) (handled bool, err error) {
	switch req.GetAction() {
	case "patches_list":
		return true, handleList(ctx, s, req, stream)
	case "patches_upsert":
		return true, handleUpsert(ctx, s, req, stream)
	case "patches_delete":
		return true, handleDelete(ctx, s, req, stream)
	case "patches_reorder":
		return true, handleReorder(ctx, s, req, stream)
	}
	return false, nil
}

// InWorkflow reports whether req carries a real (workflow_id, step_id)
// scope rather than an ad-hoc invocation.
func InWorkflow(req *pluginpb.ExecuteRequest) bool {
	return req.GetWorkflowId() != "" && req.GetStepId() != "" && req.GetStepId() != "ad-hoc"
}

func requireScope(s *Store, req *pluginpb.ExecuteRequest) (workflowID, stepID string, err error) {
	if !InWorkflow(req) {
		return "", "", fmt.Errorf("workflow_id and step_id are required for patch operations; open a step's config from a workflow DAG")
	}
	if s == nil {
		return "", "", errors.New("plugin store is not configured; set --pg-dsn or PG_DSN")
	}
	return req.GetWorkflowId(), req.GetStepId(), nil
}

func handleList(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := requireScope(s, req)
	if err != nil {
		return err
	}
	patches, err := s.ListPatches(ctx, wf, st)
	if err != nil {
		return fmt.Errorf("list patches: %w", err)
	}
	return execstream.SendFinalJSON(stream, map[string]any{"patches": patches})
}

// upsertPayload is what the MFE sends. patch_id is optional - a
// missing one triggers a fresh random ID so the same "Add patch" click
// never accidentally clobbers a sibling. Ordinal is optional too and
// defaults to (max+1) inside the store when unset (see UpsertPatch).
type upsertPayload struct {
	PatchID   string `json:"patch_id"`
	Name      string `json:"name"`
	TargetDir string `json:"target_dir"`
	Content   string `json:"content"`
	Ordinal   int    `json:"ordinal"`
}

func handleUpsert(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := requireScope(s, req)
	if err != nil {
		return err
	}
	var body upsertPayload
	if raw := req.GetPayload(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("decode patch payload: %w", err)
		}
	}
	if body.Name == "" {
		return errors.New("name is required")
	}
	if body.TargetDir == "" {
		return errors.New("target_dir is required")
	}
	if body.Content == "" {
		return errors.New("content is required")
	}
	if body.PatchID == "" {
		id, err := newPatchID()
		if err != nil {
			return fmt.Errorf("generate patch id: %w", err)
		}
		body.PatchID = id
	}
	saved, err := s.UpsertPatch(ctx, Patch{
		WorkflowID: wf,
		StepID:     st,
		PatchID:    body.PatchID,
		Name:       body.Name,
		TargetDir:  body.TargetDir,
		Content:    body.Content,
		Ordinal:    body.Ordinal,
	})
	if err != nil {
		return fmt.Errorf("upsert patch: %w", err)
	}
	return execstream.SendFinalJSON(stream, map[string]any{"patch": saved})
}

func handleDelete(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := requireScope(s, req)
	if err != nil {
		return err
	}
	var body struct {
		PatchID string `json:"patch_id"`
	}
	if raw := req.GetPayload(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
	}
	if body.PatchID == "" {
		return errors.New("patch_id is required")
	}
	if err := s.DeletePatch(ctx, wf, st, body.PatchID); err != nil {
		return fmt.Errorf("delete patch: %w", err)
	}
	return execstream.SendFinalJSON(stream, map[string]any{"deleted": body.PatchID})
}

func handleReorder(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := requireScope(s, req)
	if err != nil {
		return err
	}
	var body struct {
		OrderedPatchIDs []string `json:"ordered_patch_ids"`
	}
	if raw := req.GetPayload(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
	}
	if len(body.OrderedPatchIDs) == 0 {
		return errors.New("ordered_patch_ids must be non-empty")
	}
	if err := s.ReorderPatches(ctx, wf, st, body.OrderedPatchIDs); err != nil {
		return fmt.Errorf("reorder patches: %w", err)
	}
	patches, err := s.ListPatches(ctx, wf, st)
	if err != nil {
		return fmt.Errorf("list patches after reorder: %w", err)
	}
	return execstream.SendFinalJSON(stream, map[string]any{"patches": patches})
}

func newPatchID() (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "p_" + hex.EncodeToString(buf[:]), nil
}

// LoadByDir fetches a step's patches grouped by cleaned target dir.
// Returns an empty map for ad-hoc invocations (no workflow context) - a
// queue keyed by (workflow_id, step_id) doesn't apply outside a
// workflow. Any DB error is returned so the caller fails the run loudly
// rather than silently dropping patches the author expected to land.
func LoadByDir(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest) (map[string][]Patch, error) {
	out := map[string][]Patch{}
	if s == nil || !InWorkflow(req) {
		return out, nil
	}
	all, err := s.ListPatches(ctx, req.GetWorkflowId(), req.GetStepId())
	if err != nil {
		return nil, fmt.Errorf("load patches for (%s,%s): %w", req.GetWorkflowId(), req.GetStepId(), err)
	}
	for _, pt := range all {
		key := filepath.Clean(pt.TargetDir)
		out[key] = append(out[key], pt)
	}
	return out, nil
}

// ApplyOptions customise how ApplyPatch commits.
type ApplyOptions struct {
	// Message builds the commit message for a plain unified diff. mbox
	// patches keep the message baked into the patch.
	Message func(dirPath string, pt Patch) string
	// Trailer, when non-empty (e.g. "Rebase-Patch-Id: p_…"), is added
	// to every commit the patch produces - including each commit of a
	// multi-patch mbox - so later tooling can recognise queue-applied
	// commits.
	Trailer string
}

// ApplyPatch drops one attached patch onto the working tree. Two shapes
// are supported so authors don't have to strip mbox headers by hand:
//
//   - mbox (git format-patch output, starts with "From <sha> …"): use
//     `git am --3way`, which preserves the author, date, and commit
//     message from the patch itself.
//   - plain unified diff: `git apply --3way` to stage the change, then
//     `git commit` with a plugin-generated message that names the patch.
//
// Both are run from the repo root so the patch's file paths can span
// the whole tree, even when the patch is attached to a nested dir.
//
// Empty diffs (nothing to change) are treated as a hard error - a patch
// that no-ops probably means the caller pointed it at the wrong dir /
// step, and silently ignoring it would mask that.
func ApplyPatch(r *runner.Runner, repoDir, dirCwd, dirPath string, pt Patch, opts ApplyOptions) error {
	// The scratch file goes inside the git dir, never the worktree: the
	// plain-diff path below runs `git add -A` before committing, which
	// would otherwise stage the patch file itself into the commit.
	gitDir, err := r.Output(repoDir, "git", "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("locate git dir: %w", err)
	}
	scratch, err := os.CreateTemp(strings.TrimSpace(gitDir), "attached-*.patch")
	if err != nil {
		return fmt.Errorf("write patch to scratch file: %w", err)
	}
	patchPath := scratch.Name()
	defer os.Remove(patchPath)
	content := pt.Content
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if _, err := scratch.WriteString(content); err != nil {
		scratch.Close()
		return fmt.Errorf("write patch content: %w", err)
	}
	if err := scratch.Close(); err != nil {
		return fmt.Errorf("close patch scratch: %w", err)
	}

	if LooksLikeMbox(pt.Content) {
		before, err := r.Output(repoDir, "git", "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		before = strings.TrimSpace(before)
		// `git am` sets committer identity from user.email / user.name,
		// which the plugin configured on the repo just after clone.
		if err := r.Run(repoDir, "git", "am", "--3way", "--keep-cr", patchPath); err != nil {
			// Leave the tree in a recoverable state so the failure message
			// isn't muddled by "you have unresolved mbox state" on retry.
			_, _ = r.ExitCode(repoDir, "git", "am", "--abort")
			return fmt.Errorf("git am: %w", err)
		}
		if opts.Trailer != "" {
			// Tag every commit the mbox produced. rebase --exec is
			// non-interactive without -i and handles 1..N commits alike.
			amend := "git commit --amend --no-edit --trailer " + shellQuote(opts.Trailer)
			if err := r.Run(repoDir, "git", "rebase", "--exec", amend, before); err != nil {
				_, _ = r.ExitCode(repoDir, "git", "rebase", "--abort")
				return fmt.Errorf("add trailer to patch commits: %w", err)
			}
		}
		r.Note(fmt.Sprintf("# [%s] applied patch %q via git am\n", dirPath, pt.Name))
		return nil
	}

	if err := r.Run(repoDir, "git", "apply", "--3way", "--index", patchPath); err != nil {
		return fmt.Errorf("git apply: %w", err)
	}
	if err := r.Run(repoDir, "git", "add", "-A"); err != nil {
		return fmt.Errorf("git add after patch: %w", err)
	}
	hasChanges, err := r.HasStagedChanges(repoDir)
	if err != nil {
		return fmt.Errorf("check staged changes after patch: %w", err)
	}
	if !hasChanges {
		return fmt.Errorf("patch %q produced no staged changes - is it the right patch for this step?", pt.Name)
	}
	args := []string{"commit", "-m", opts.Message(dirPath, pt)}
	if opts.Trailer != "" {
		args = append(args, "--trailer", opts.Trailer)
	}
	if err := r.Run(dirCwd, "git", args...); err != nil {
		return fmt.Errorf("git commit patch: %w", err)
	}
	r.Note(fmt.Sprintf("# [%s] applied patch %q via git apply\n", dirPath, pt.Name))
	return nil
}

// LooksLikeMbox tells apart `git format-patch` output (mbox) from a
// plain unified diff. Mbox always begins with "From <sha> Mon Sep 17…"
// on its very first line, which is unambiguous - plain diffs start with
// "diff --git" or "---". Anything else falls through to `git apply`,
// which surfaces its own parse error to the caller.
func LooksLikeMbox(s string) bool {
	trimmed := strings.TrimLeft(s, "\r\n\t ")
	return strings.HasPrefix(trimmed, "From ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
