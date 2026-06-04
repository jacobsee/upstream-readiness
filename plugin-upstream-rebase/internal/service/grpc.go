// Package service implements the upstream-rebase plugin's gRPC
// contract.
//
// The `rebase` action re-applies a fork's UPSTREAM commits onto a new
// upstream release the OpenShift way:
//
//	git checkout -b rebase/<tag> <tag>
//	git merge -s ours <old fork branch>
//	git cherry-pick <every UPSTREAM: <carry>/<PR> commit since the previous rebase>
//
// then runs the same dependency / patch-queue / make pipeline as
// generic-go-dep (pluginkit/godeps), then the step's bare shell
// commands (each committed on its own), and force-pushes to a fork.
//
// Conflicts and commits that can't be handled mechanically suspend the
// step (ExecuteSuspend); the plugin's "resolution" MFE slot collects the
// answer, which is stored in this plugin's own Postgres and re-applied
// automatically whenever the same conflict recurs. See rebase.go.
package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/cmdlog"
	"github.com/jacobsee/upstream-readiness/pluginkit/gitenv"
	"github.com/jacobsee/upstream-readiness/pluginkit/godeps"
	"github.com/jacobsee/upstream-readiness/pluginkit/mfeassets"
	"github.com/jacobsee/upstream-readiness/pluginkit/patchqueue"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// Rebaser carries the plugin's shared handles.
type Rebaser struct {
	pluginpb.UnimplementedPluginServer
	Assets *mfeassets.Server
	Store  *store.Store
	// WorkRoot holds per-(run, step) working copies kept while a step
	// is suspended, so a resume can continue in place. Purely a cache:
	// if a working copy is missing or doesn't match the checkpoint, the
	// rebase replays from scratch using the stored resolutions.
	WorkRoot string
}

func (s *Rebaser) GetAsset(req *pluginpb.GetAssetRequest, stream pluginpb.Plugin_GetAssetServer) error {
	return s.Assets.GetAsset(req, stream)
}

func (s *Rebaser) Describe(_ context.Context, _ *pluginpb.DescribeRequest) (*pluginpb.PluginManifest, error) {
	actions := []*pluginpb.ActionDescriptor{
		{
			Name:        "rebase",
			Description: "Rebase a fork onto a new upstream release: merge -s ours, re-pick every UPSTREAM carry/PR commit, run dependency updates, make targets, then shell commands, push to a fork.",
			Outputs: []*pluginpb.ActionOutput{
				{Name: "ref", Description: "Branch name pushed to the fork."},
				{Name: "commit", Description: "Full SHA of the tip commit pushed to the fork."},
				{Name: "repo", Description: "The fork repository URL the rebase was based on (clone_url)."},
				{Name: "fork_url", Description: "Fork URL the branch was pushed to."},
				{Name: "target_ref", Description: "The upstream ref the fork was rebased onto."},
				{Name: "target_commit", Description: "SHA of target_ref."},
				{Name: "previous_rebase_commit", Description: "The previous `merge -s ours` commit the carries were collected after."},
				{Name: "bumps", Description: "Applied dependency bump records ({path, dependency, target_version, replace?})."},
				{Name: "commits", Description: "Every commit considered: [{orig_sha, new_sha, subject, kind, pr, status}]."},
			},
		},
		// MFE-backing actions: called ad-hoc via pluginApi.execute with
		// workflow / step / run scoping, never from workflow YAML.
		{Name: "run_get", Description: "Commit table + run state for a (workflow_run_id, step_id)."},
		{Name: "conflict_get", Description: "A captured conflict with every side of every file, plus hints."},
		{Name: "resolution_save", Description: "Save a resolution for a conflict (reused for identical conflicts in later runs)."},
		{Name: "decisions_save", Description: "Save decisions for commits that need a human (unclassified, malformed, evil merges, empty carries)."},
		{Name: "resolutions_list", Description: "List a step's saved resolutions."},
		{Name: "resolutions_delete", Description: "Delete a saved resolution and the rerere entries it produced."},
		{Name: "decisions_list", Description: "List a step's saved decisions."},
		{Name: "decisions_delete", Description: "Delete a saved decision."},
		{Name: "rerere_list", Description: "List a step's stored git rerere entries and what produced them."},
		{Name: "memory_delete", Description: "Delete selected resolutions (with their rerere entries), decisions, and rerere entries of a step."},
		{Name: "memory_wipe", Description: "Wipe selected categories of memory for a step or for every step of the workflow."},
	}
	actions = append(actions, patchqueue.ActionDescriptors()...)
	actions = append(actions, cmdlog.ActionDescriptors()...)
	return &pluginpb.PluginManifest{
		Id:          "upstream-rebase",
		DisplayName: "Upstream Rebase",
		Version:     "0.1.0",
		MfeEntry:    s.Assets.Entry(),
		MfeSlots: []*pluginpb.MfeSlot{
			{Slot: "instance-config"},
			{Slot: "instance-list-item"},
			{Slot: "step-runtime"},
			{Slot: "resolution"},
			{Slot: "workflow-step-config"},
		},
		Actions: actions,
	}, nil
}

func (s *Rebaser) Execute(req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	ctx := stream.Context()
	switch req.GetAction() {
	case "rebase":
		return s.handleRebase(ctx, req, stream)
	case "run_get":
		return s.handleRunGet(ctx, req, stream)
	case "conflict_get":
		return s.handleConflictGet(ctx, req, stream)
	case "resolution_save":
		return s.handleResolutionSave(ctx, req, stream)
	case "decisions_save":
		return s.handleDecisionsSave(ctx, req, stream)
	case "resolutions_list":
		return s.handleResolutionsList(ctx, req, stream)
	case "resolutions_delete":
		return s.handleResolutionsDelete(ctx, req, stream)
	case "decisions_list":
		return s.handleDecisionsList(ctx, req, stream)
	case "decisions_delete":
		return s.handleDecisionsDelete(ctx, req, stream)
	case "rerere_list":
		return s.handleRerereList(ctx, req, stream)
	case "memory_delete":
		return s.handleMemoryDelete(ctx, req, stream)
	case "memory_wipe":
		return s.handleMemoryWipe(ctx, req, stream)
	}
	var patches *patchqueue.Store
	var commands *cmdlog.Store
	if s.Store != nil {
		patches, commands = s.Store.Patches, s.Store.Commands
	}
	if handled, err := patchqueue.Handle(ctx, patches, req, stream); handled {
		return err
	}
	if handled, err := cmdlog.Handle(ctx, commands, req, stream); handled {
		return err
	}
	return fmt.Errorf("unknown action %q", req.GetAction())
}

func (s *Rebaser) Validate(_ context.Context, req *pluginpb.ValidateRequest) (*pluginpb.ValidateResponse, error) {
	switch req.GetKind() {
	case "instance":
		var cfg gitenv.InstanceConfig
		if raw := req.GetPayload(); len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return &pluginpb.ValidateResponse{Valid: false, Errors: []string{"invalid JSON: " + err.Error()}}, nil
			}
		}
		errs := gitenv.ValidateInstance(cfg)
		return &pluginpb.ValidateResponse{Valid: len(errs) == 0, Errors: errs}, nil
	case "step":
		errs := validateStepRaw(req.GetPayload())
		return &pluginpb.ValidateResponse{Valid: len(errs) == 0, Errors: errs}, nil
	default:
		return &pluginpb.ValidateResponse{Valid: false, Errors: []string{fmt.Sprintf("unknown validate kind %q", req.GetKind())}}, nil
	}
}

// validateStepRaw checks key presence only - values may still be __ref
// markers at edit time.
func validateStepRaw(payload []byte) []string {
	var raw map[string]any
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &raw); err != nil {
			return []string{"invalid JSON: " + err.Error()}
		}
	}
	var errs []string
	for _, k := range []string{"clone_url", "upstream_url", "target_ref"} {
		if _, ok := raw[k]; !ok {
			errs = append(errs, k+" is required")
		}
	}
	if v, ok := raw["rerere"]; ok {
		if m, _ := v.(string); !validRerere(m) {
			errs = append(errs, `rerere must be one of "off", "suggest", "auto"`)
		}
	}
	if dirsRaw, present := raw["dirs"]; present {
		errs = append(errs, godeps.ValidateRaw(dirsRaw, true, godeps.Options{AllowNoUpdates: true})...)
	}
	if cmdsRaw, present := raw["commands"]; present {
		errs = append(errs, validateCommandsRaw(cmdsRaw)...)
	}
	return errs
}

func validateCommandsRaw(cmdsRaw any) []string {
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

func validRerere(m string) bool {
	return m == "off" || m == "suggest" || m == "auto"
}
