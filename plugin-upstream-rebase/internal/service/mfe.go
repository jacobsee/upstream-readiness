package service

// MFE-backing actions. The plugin's own UI (step-runtime, resolution,
// workflow-step-config slots) calls these through pluginApi.execute with
// workflow_id / step_id / workflow_run_id scoping lifted into the
// ExecuteRequest envelope; storage is the plugin's own Postgres.
//
// These same actions are the seam a future AI resolver plugs into: it
// reads conflict_get and writes resolution_save / decisions_save (with
// resolved_by "ai:<model>") exactly as a human does through the UI.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/gitrebase"
	"github.com/jacobsee/upstream-readiness/plugin-upstream-rebase/internal/store"
	"github.com/jacobsee/upstream-readiness/pluginkit/execstream"
	"github.com/jacobsee/upstream-readiness/pluginkit/patchqueue"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// Conflict markers at the start of a line, as git writes them.
var (
	conflictStart = regexp.MustCompile(`(?m)^<{7}( |$)`)
	conflictEnd   = regexp.MustCompile(`(?m)^>{7}( |$)`)
)

func decodePayload(req *pluginpb.ExecuteRequest, v any) error {
	if raw := req.GetPayload(); len(raw) > 0 {
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
	}
	return nil
}

func (s *Rebaser) requireStep(req *pluginpb.ExecuteRequest) (workflowID, stepID string, err error) {
	if s.Store == nil {
		return "", "", errors.New("plugin store is not configured; set --pg-dsn or PG_DSN")
	}
	if !patchqueue.InWorkflow(req) {
		return "", "", errors.New("workflow_id and step_id are required; open this from a workflow")
	}
	return req.GetWorkflowId(), req.GetStepId(), nil
}

// run_get: the commit table and run state. Scoped by workflow_run_id +
// step_id (the step-runtime and resolution slots pass runId).
func (s *Rebaser) handleRunGet(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	runID := req.GetWorkflowRunId()
	if runID == "" || runID == "ad-hoc" {
		return errors.New("workflow_run_id is required")
	}
	run, err := s.Store.GetRun(ctx, runID, st)
	if err != nil {
		return err
	}
	if run == nil {
		return execstream.SendFinalJSON(stream, map[string]any{"run": nil, "commits": []store.CommitRow{}})
	}
	commits, err := s.Store.ListCommits(ctx, runID, st)
	if err != nil {
		return err
	}
	openConflict, err := s.Store.OpenConflictID(ctx, runID, st)
	if err != nil {
		return err
	}
	decisions, err := s.Store.ListDecisions(ctx, wf, st)
	if err != nil {
		return err
	}
	byKey := map[string]store.Decision{}
	for _, d := range decisions {
		byKey[decisionKey(d.OrigSHA, d.Question)] = d
	}
	// Rows awaiting a decision, with the question each one asks and any
	// answer already saved (so the form can pre-fill).
	type pendingDecision struct {
		store.CommitRow
		Question string          `json:"question"`
		Saved    *store.Decision `json:"saved,omitempty"`
	}
	pending := []pendingDecision{}
	for _, c := range commits {
		if c.Status != stAwaitingDecision {
			continue
		}
		q := qClassify
		if !gitrebase.NeedsClassification(gitrebase.Kind(c.Kind)) {
			q = qEmpty
		}
		pd := pendingDecision{CommitRow: c, Question: q}
		if d, ok := byKey[decisionKey(c.OrigSHA, q)]; ok {
			pd.Saved = &d
		}
		pending = append(pending, pd)
	}
	return execstream.SendFinalJSON(stream, map[string]any{
		"run":              run,
		"commits":          commits,
		"open_conflict_id": openConflict,
		"pending":          pending,
	})
}

// conflict_get: every side of every conflicted file, the commit, the
// saved resolution for this exact fingerprint (if one exists yet), and
// earlier resolutions of the same commit subject as hints.
func (s *Rebaser) handleConflictGet(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	var body struct {
		ConflictID string `json:"conflict_id"`
	}
	if err := decodePayload(req, &body); err != nil {
		return err
	}
	if s.Store == nil {
		return errors.New("plugin store is not configured")
	}
	c, err := s.Store.GetConflict(ctx, body.ConflictID)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("unknown conflict %q", body.ConflictID)
	}
	saved, err := s.Store.FindResolution(ctx, c.WorkflowID, c.StepID, c.Fingerprint)
	if err != nil {
		return err
	}
	hints, err := s.Store.ListResolutions(ctx, c.WorkflowID, c.StepID, c.Subject)
	if err != nil {
		return err
	}
	filtered := hints[:0]
	for _, h := range hints {
		if h.Fingerprint != c.Fingerprint {
			filtered = append(filtered, h)
		}
	}
	return execstream.SendFinalJSON(stream, map[string]any{
		"conflict": c,
		"saved":    saved,
		"hints":    filtered,
	})
}

// resolution_save: answer a conflict. kind "files" must cover every
// conflicted path; kind "skip" drops the commit and needs a reason.
func (s *Rebaser) handleResolutionSave(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	var body struct {
		ConflictID string                              `json:"conflict_id"`
		Kind       string                              `json:"kind"`
		Files      map[string]gitrebase.FileResolution `json:"files"`
		Reason     string                              `json:"reason"`
		ResolvedBy string                              `json:"resolved_by"`
	}
	if err := decodePayload(req, &body); err != nil {
		return err
	}
	if s.Store == nil {
		return errors.New("plugin store is not configured")
	}
	c, err := s.Store.GetConflict(ctx, body.ConflictID)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("unknown conflict %q", body.ConflictID)
	}
	var conflictFiles []gitrebase.ConflictFile
	if err := json.Unmarshal(c.Files, &conflictFiles); err != nil {
		return fmt.Errorf("decode conflict files: %w", err)
	}
	switch body.Kind {
	case "skip":
		if strings.TrimSpace(body.Reason) == "" {
			return errors.New("a reason is required to skip a commit")
		}
		body.Files = nil
	case "files":
		var missing []string
		for _, f := range conflictFiles {
			fr, ok := body.Files[f.Path]
			if !ok {
				missing = append(missing, f.Path)
				continue
			}
			if fr.Content == nil && !fr.Deleted && fr.Take != "ours" && fr.Take != "theirs" {
				return fmt.Errorf("resolution for %s must set content, deleted, or take=ours|theirs", f.Path)
			}
			if f.Binary && fr.Content != nil {
				return fmt.Errorf("%s is binary: choose ours, theirs, or delete", f.Path)
			}
			if fr.Content != nil && conflictStart.MatchString(*fr.Content) && conflictEnd.MatchString(*fr.Content) {
				return fmt.Errorf("%s still contains conflict markers", f.Path)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("resolution must cover every conflicted path; missing: %s", strings.Join(missing, ", "))
		}
	default:
		return errors.New(`kind must be "files" or "skip"`)
	}
	files, err := json.Marshal(body.Files)
	if err != nil {
		return err
	}
	if body.ResolvedBy == "" {
		body.ResolvedBy = "human"
	}
	saved, err := s.Store.UpsertResolution(ctx, store.Resolution{
		WorkflowID: c.WorkflowID, StepID: c.StepID, Fingerprint: c.Fingerprint, Subject: c.Subject, OrigSHA: c.OrigSHA,
		Kind: body.Kind, Files: files, Reason: body.Reason, ResolvedBy: body.ResolvedBy, CreatedInRun: c.WorkflowRunID,
	})
	if err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"resolution": saved})
}

// decisions_save: answers for rows awaiting a decision.
func (s *Rebaser) handleDecisionsSave(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	var body struct {
		Decisions []struct {
			OrigSHA  string `json:"orig_sha"`
			Question string `json:"question"`
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
			Subject  string `json:"subject"`
		} `json:"decisions"`
		DecidedBy string `json:"decided_by"`
	}
	if err := decodePayload(req, &body); err != nil {
		return err
	}
	if len(body.Decisions) == 0 {
		return errors.New("decisions must be non-empty")
	}
	if body.DecidedBy == "" {
		body.DecidedBy = "human"
	}
	// Record each commit's subject from the run's own table when the
	// caller is scoped to a run, so subject-based carry reuse (see
	// job.gate) doesn't depend on the client echoing it correctly.
	subjects := map[string]string{}
	if runID := req.GetWorkflowRunId(); runID != "" && runID != "ad-hoc" {
		rows, err := s.Store.ListCommits(ctx, runID, st)
		if err != nil {
			return err
		}
		for _, r := range rows {
			subjects[r.OrigSHA] = r.Subject
		}
	}
	for i := range body.Decisions {
		if subj, ok := subjects[body.Decisions[i].OrigSHA]; ok {
			body.Decisions[i].Subject = subj
		}
	}
	for _, d := range body.Decisions {
		if d.OrigSHA == "" {
			return errors.New("orig_sha is required")
		}
		if strings.TrimSpace(d.Reason) == "" {
			return fmt.Errorf("a reason is required for %s", short(d.OrigSHA))
		}
		switch d.Question {
		case qClassify:
			if d.Decision != "carry" && d.Decision != "drop" {
				return fmt.Errorf("classify decision for %s must be carry or drop", short(d.OrigSHA))
			}
		case qEmpty:
			if d.Decision != "drop" && d.Decision != "keep_empty" {
				return fmt.Errorf("empty-commit decision for %s must be drop or keep_empty", short(d.OrigSHA))
			}
		default:
			return fmt.Errorf("unknown question %q", d.Question)
		}
	}
	for _, d := range body.Decisions {
		if err := s.Store.UpsertDecision(ctx, store.Decision{
			WorkflowID: wf, StepID: st, OrigSHA: d.OrigSHA, Question: d.Question, Decision: d.Decision,
			Reason: d.Reason, Subject: d.Subject, DecidedBy: body.DecidedBy,
		}); err != nil {
			return err
		}
	}
	return execstream.SendFinalJSON(stream, map[string]any{"saved": len(body.Decisions)})
}

func (s *Rebaser) handleResolutionsList(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	list, err := s.Store.ListResolutions(ctx, wf, st, "")
	if err != nil {
		return err
	}
	n, err := s.Store.CountRerere(ctx, wf, st)
	if err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"resolutions": list, "rerere_entries": n})
}

func (s *Rebaser) handleResolutionsDelete(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	var body struct {
		ResolutionID string `json:"resolution_id"`
	}
	if err := decodePayload(req, &body); err != nil {
		return err
	}
	if body.ResolutionID == "" {
		return errors.New("resolution_id is required")
	}
	n, rr, err := s.Store.DeleteResolutions(ctx, wf, st, []string{body.ResolutionID})
	if err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"deleted": map[string]int64{"resolutions": n, "rerere": rr}})
}

func (s *Rebaser) handleDecisionsList(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	list, err := s.Store.ListDecisions(ctx, wf, st)
	if err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"decisions": list})
}

func (s *Rebaser) handleDecisionsDelete(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	var body struct {
		OrigSHA  string `json:"orig_sha"`
		Question string `json:"question"`
	}
	if err := decodePayload(req, &body); err != nil {
		return err
	}
	if body.OrigSHA == "" || body.Question == "" {
		return errors.New("orig_sha and question are required")
	}
	if err := s.Store.DeleteDecision(ctx, wf, st, body.OrigSHA, body.Question); err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"deleted": body.OrigSHA})
}

// rerere_list: a step's rerere entries (metadata only) and what
// produced each one.
func (s *Rebaser) handleRerereList(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	list, err := s.Store.ListRerereMeta(ctx, wf, st)
	if err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"entries": list})
}

// memory_delete: selective deletion of a step's memory. Deleting a
// resolution also deletes the rerere entries it produced.
func (s *Rebaser) handleMemoryDelete(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	var body struct {
		ResolutionIDs []string            `json:"resolution_ids"`
		Decisions     []store.DecisionKey `json:"decisions"`
		RerereIDs     []string            `json:"rerere_ids"`
	}
	if err := decodePayload(req, &body); err != nil {
		return err
	}
	if len(body.ResolutionIDs)+len(body.Decisions)+len(body.RerereIDs) == 0 {
		return errors.New("nothing selected")
	}
	counts := map[string]int64{}
	res, rr, err := s.Store.DeleteResolutions(ctx, wf, st, body.ResolutionIDs)
	if err != nil {
		return err
	}
	counts["resolutions"], counts["rerere"] = res, rr
	n, err := s.Store.DeleteRerere(ctx, wf, st, body.RerereIDs)
	if err != nil {
		return err
	}
	counts["rerere"] += n
	if counts["decisions"], err = s.Store.DeleteDecisions(ctx, wf, st, body.Decisions); err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"deleted": counts})
}

// memory_wipe: wipe selected categories for this step, or (scope
// "workflow") for every step of the workflow.
func (s *Rebaser) handleMemoryWipe(ctx context.Context, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	wf, st, err := s.requireStep(req)
	if err != nil {
		return err
	}
	var body struct {
		Scope      string            `json:"scope"` // step | workflow
		Categories store.WipeOptions `json:"categories"`
	}
	if err := decodePayload(req, &body); err != nil {
		return err
	}
	c := body.Categories
	if !c.Resolutions && !c.Decisions && !c.Rerere && !c.Patches && !c.RunHistory {
		return errors.New("select at least one category to wipe")
	}
	switch body.Scope {
	case "step", "":
	case "workflow":
		st = ""
	default:
		return errors.New(`scope must be "step" or "workflow"`)
	}
	counts, err := s.Store.Wipe(ctx, wf, st, c)
	if err != nil {
		return err
	}
	return execstream.SendFinalJSON(stream, map[string]any{"deleted": counts})
}
