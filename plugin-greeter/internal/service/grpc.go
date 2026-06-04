// Package service holds the greeter plugin's gRPC implementation. Its
// micro-frontend bundle is served over the same gRPC listener
// (GetAsset), proxied to the browser by the orchestrator.
package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jacobsee/upstream-readiness/pluginkit/mfeassets"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// Greeter implements the shared Plugin gRPC contract. The instance
// config holds a default suffix the greeter appends to every greeting,
// so different instances can carry different personalities without the
// orchestrator knowing what a suffix is.
type Greeter struct {
	pluginpb.UnimplementedPluginServer
	Assets *mfeassets.Server
}

func (g *Greeter) GetAsset(req *pluginpb.GetAssetRequest, stream pluginpb.Plugin_GetAssetServer) error {
	return g.Assets.GetAsset(req, stream)
}

func (g *Greeter) Describe(_ context.Context, _ *pluginpb.DescribeRequest) (*pluginpb.PluginManifest, error) {
	return &pluginpb.PluginManifest{
		Id:          "greeter",
		DisplayName: "Greeter",
		Version:     "0.2.0",
		MfeEntry:    g.Assets.Entry(),
		MfeElement:  "",
		MfeSlots: []*pluginpb.MfeSlot{
			{Slot: "action"},
			{Slot: "instance-config"},
			{Slot: "step-runtime"},
			{Slot: "sidebar-badge"},
			{Slot: "plugin-home"},
		},
		Actions: []*pluginpb.ActionDescriptor{
			{
				Name:        "greet",
				Description: "Return a greeting for a given name.",
				Outputs: []*pluginpb.ActionOutput{
					{Name: "greeted_name", Description: "The name that was greeted."},
				},
			},
		},
	}, nil
}

type greeterInstance struct {
	Suffix string `json:"suffix"`
}

type greetPayload struct {
	Name string `json:"name"`
}

type greetResult struct {
	Message string `json:"message"`
}

func (g *Greeter) Execute(req *pluginpb.ExecuteRequest, stream pluginpb.Plugin_ExecuteServer) error {
	if req.GetAction() != "greet" {
		return fmt.Errorf("unknown action %q", req.GetAction())
	}

	var cfg greeterInstance
	if blob := req.GetInstanceConfig(); len(blob) > 0 {
		_ = json.Unmarshal(blob, &cfg)
	}
	var p greetPayload
	if len(req.GetPayload()) > 0 {
		if err := json.Unmarshal(req.GetPayload(), &p); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
	}
	if p.Name == "" {
		p.Name = "stranger"
	}
	// Trivial progress event so the step-runtime slot has something to
	// render while the (near-instant) greeting is being composed.
	prog, _ := json.Marshal(map[string]string{"phase": "greeting", "name": p.Name})
	_ = stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Progress{Progress: &pluginpb.ExecuteProgress{Payload: prog}},
	})

	msg := fmt.Sprintf("Hello, %s!", p.Name)
	if cfg.Suffix != "" {
		msg = msg + " " + cfg.Suffix
	}

	resultJSON, _ := json.Marshal(greetResult{Message: msg})
	nameJSON, _ := json.Marshal(p.Name)
	return stream.Send(&pluginpb.ExecuteEvent{
		Event: &pluginpb.ExecuteEvent_Final{Final: &pluginpb.ExecuteFinal{
			Result: resultJSON,
			Outputs: &pluginpb.StepOutputs{
				Values: map[string][]byte{"greeted_name": nameJSON},
			},
		}},
	})
}

func (g *Greeter) Validate(_ context.Context, req *pluginpb.ValidateRequest) (*pluginpb.ValidateResponse, error) {
	// The greeter accepts any instance config (suffix is optional) and
	// any step params blob.
	return &pluginpb.ValidateResponse{Valid: true}, nil
}
