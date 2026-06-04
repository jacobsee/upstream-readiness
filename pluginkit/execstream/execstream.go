// Package execstream holds tiny helpers for ending a plugin's Execute
// stream. Progress payload shapes stay plugin-owned; these only wrap the
// terminal events.
package execstream

import (
	"encoding/json"

	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// SendFinalJSON ends the stream with a Final event whose result is v
// marshalled as JSON and no outputs - the shape the plugins' MFE-backing
// actions (patches_*, run_get, …) return to pluginApi.execute.
func SendFinalJSON(stream pluginpb.Plugin_ExecuteServer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Final{Final: &pluginpb.ExecuteFinal{Result: body}},
	})
}

// SendSuspendJSON ends the stream with a Suspend event whose prompt is v
// marshalled as JSON. The orchestrator parks the step and mounts the
// plugin's "resolution" slot with the prompt; Execute is called again
// with ExecuteRequest.resolution once the slot answers.
func SendSuspendJSON(stream pluginpb.Plugin_ExecuteServer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Suspend{Suspend: &pluginpb.ExecuteSuspend{Prompt: body}},
	})
}
