package cmdlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jacobsee/upstream-readiness/pluginkit/execstream"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ActionDescriptors are the manifest entries for the cmdlog_* actions.
// They back the shared CommandHistory MFE component; the MFE calls them
// via pluginApi.execute with { workflowId, stepId, runId } scoping.
func ActionDescriptors() []*pluginpb.ActionDescriptor {
	return []*pluginpb.ActionDescriptor{
		{Name: "cmdlog_list", Description: "Page through every command, query, and note a (workflow_run_id, step_id) ran, without output bodies."},
		{Name: "cmdlog_get", Description: "One recorded command with its full output."},
	}
}

// Handle serves the cmdlog_* actions. handled is false for any other
// action so the plugin's own router can carry on. Scope comes from the
// request envelope (workflow_run_id, step_id); ad-hoc invocations are
// scoped to run "ad-hoc" / step "ad-hoc".
func Handle(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) (handled bool, err error) {
	switch req.GetAction() {
	case "cmdlog_list":
		return true, handleList(ctx, s, req, stream)
	case "cmdlog_get":
		return true, handleGet(ctx, s, req, stream)
	}
	return false, nil
}

func scope(s *Store, req *pluginpb.ExecuteRequest) (runID, stepID string, err error) {
	if s == nil {
		return "", "", status.Error(codes.FailedPrecondition, "plugin store is not configured; set --pg-dsn or PG_DSN")
	}
	if req.GetWorkflowRunId() == "" || req.GetStepId() == "" {
		return "", "", status.Error(codes.InvalidArgument, "workflow_run_id and step_id are required")
	}
	return req.GetWorkflowRunId(), req.GetStepId(), nil
}

func decode(req *pluginpb.ExecuteRequest, v any) error {
	if raw := req.GetPayload(); len(raw) > 0 {
		if err := json.Unmarshal(raw, v); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode payload: %v", err)
		}
	}
	return nil
}

func handleList(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	runID, stepID, err := scope(s, req)
	if err != nil {
		return err
	}
	var in struct {
		AfterID int64    `json:"after_id"`
		Limit   int      `json:"limit"`
		Kinds   []string `json:"kinds"`
		Query   string   `json:"q"`
	}
	if err := decode(req, &in); err != nil {
		return err
	}
	page, err := s.List(ctx, runID, stepID, ListOptions{AfterID: in.AfterID, Limit: in.Limit, Kinds: in.Kinds, Query: in.Query})
	if err != nil {
		return fmt.Errorf("list command history: %w", err)
	}
	return execstream.SendFinalJSON(stream, page)
}

func handleGet(ctx context.Context, s *Store, req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	runID, stepID, err := scope(s, req)
	if err != nil {
		return err
	}
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(req, &in); err != nil {
		return err
	}
	if in.ID == 0 {
		return status.Error(codes.InvalidArgument, "id is required")
	}
	e, err := s.Get(ctx, runID, stepID, in.ID)
	if err != nil {
		return fmt.Errorf("get command: %w", err)
	}
	if e == nil {
		return status.Error(codes.NotFound, errors.New("no such command in this step's history").Error())
	}
	return execstream.SendFinalJSON(stream, map[string]any{"entry": e})
}
