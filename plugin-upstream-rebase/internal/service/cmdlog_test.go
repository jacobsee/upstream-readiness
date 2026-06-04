package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jacobsee/upstream-readiness/pluginkit/cmdlog"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

func (e *env) history(runID string, opts map[string]any) cmdlog.Page {
	e.t.Helper()
	body, _ := json.Marshal(opts)
	var p cmdlog.Page
	if err := json.Unmarshal(e.exec(runID, "cmdlog_list", body, nil, nil).terminal().GetFinal().GetResult(), &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// Every command, query, and note of every execution of a step - not
// just the progress tail - is kept and served.
func TestCommandHistory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	params := e.params("v1.1", e.fork, e.push)
	run := e.workflowID + "-history"

	// Execution 1 suspends for a decision; execution 2 (the resume)
	// suspends on the conflict.
	suspendPromptOf(t, e.exec(run, "rebase", nil, nil, params))
	misc := e.table(run)["fix a thing without a prefix"]
	dec, _ := json.Marshal(map[string]any{"decisions": []map[string]string{{
		"orig_sha": misc.OrigSHA, "question": "classify", "decision": "carry", "reason": "test",
	}}})
	e.exec(run, "decisions_save", dec, nil, nil)
	suspendPromptOf(t, e.exec(run, "rebase", nil, []byte(`{"action":"continue"}`), params))

	all := e.history(run, map[string]any{"limit": 2000})
	if all.More || len(all.Entries) == 0 {
		t.Fatalf("history: %d entries, more=%v", len(all.Entries), all.More)
	}
	execs := map[string]bool{}
	var clone, conflictPick *cmdlog.Entry
	for i := range all.Entries {
		en := &all.Entries[i]
		execs[en.ExecutionID] = true
		if en.FinishedAt == nil || en.ExitCode == nil {
			t.Errorf("entry %d (%s) never finished", en.ID, en.Command)
		}
		if strings.HasPrefix(en.Command, "git clone") && clone == nil {
			clone = en
		}
		if strings.HasPrefix(en.Command, "git cherry-pick") && en.ExitCode != nil && *en.ExitCode != 0 {
			conflictPick = en
		}
	}
	if len(execs) != 2 {
		t.Errorf("history spans %d executions, want 2 (first run + resume)", len(execs))
	}
	for _, k := range []string{"command", "query", "note"} {
		if all.Counts[k] == 0 {
			t.Errorf("no %s entries recorded; counts %v", k, all.Counts)
		}
	}
	if clone == nil || *clone.ExitCode != 0 {
		t.Fatalf("git clone not recorded as a successful command: %+v", clone)
	}
	if conflictPick == nil {
		t.Fatal("the conflicting cherry-pick was not recorded with its non-zero exit code")
	}

	// Full output of one command, scoped to its step.
	got := e.exec(run, "cmdlog_get", []byte(fmt.Sprintf(`{"id":%d}`, conflictPick.ID)), nil, nil)
	var g struct{ Entry cmdlog.Entry }
	_ = json.Unmarshal(got.terminal().GetFinal().GetResult(), &g)
	if !strings.Contains(g.Entry.Output, "CONFLICT") || int64(len(g.Entry.Output)) != g.Entry.OutputBytes {
		t.Errorf("cherry-pick output = %q (%d bytes recorded)", g.Entry.Output, g.Entry.OutputBytes)
	}
	err := e.svc.Execute(&pluginpb.ExecuteRequest{
		Action: "cmdlog_get", Payload: []byte(fmt.Sprintf(`{"id":%d}`, conflictPick.ID)),
		WorkflowRunId: run + "-other", StepId: "rebase",
	}, &fakeStream{ctx: ctx})
	if err == nil {
		t.Error("cmdlog_get served an entry outside the requested (run, step)")
	}

	// Filters and paging.
	if q := e.history(run, map[string]any{"q": "CONFLICT"}); len(q.Entries) == 0 {
		t.Error("output search found nothing")
	}
	if k := e.history(run, map[string]any{"kinds": []string{"note"}}); len(k.Entries) != all.Counts["note"] {
		t.Errorf("kind filter: %d notes, want %d", len(k.Entries), all.Counts["note"])
	}
	page := e.history(run, map[string]any{"limit": 3})
	next := e.history(run, map[string]any{"limit": 3, "after_id": page.Entries[2].ID})
	if !page.More || len(next.Entries) != 3 || next.Entries[0].ID != all.Entries[3].ID {
		t.Errorf("paging broken: first page more=%v, next starts at %d, want %d", page.More, next.Entries[0].ID, all.Entries[3].ID)
	}
}
