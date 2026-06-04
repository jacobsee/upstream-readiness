// Package registry tracks the plugins the orchestrator knows about and
// keeps an open gRPC connection to each so we can invoke their actions.
// The registry is a thin pass-through: it never inspects the opaque
// blobs flowing between the orchestrator and plugins.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type Entry struct {
	Endpoint string
	Manifest *pluginpb.PluginManifest
	client   pluginpb.PluginClient
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

func New() *Registry {
	return &Registry{entries: map[string]*Entry{}}
}

// Register dials a plugin, fetches its manifest, and indexes it by manifest ID.
// Describe is retried for the lifetime of ctx so the orchestrator can start
// before its plugins finish booting.
func (r *Registry) Register(ctx context.Context, endpoint string) error {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial %s: %w", endpoint, err)
	}
	client := pluginpb.NewPluginClient(conn)
	manifest, err := describeWithRetry(ctx, client)
	if err != nil {
		return fmt.Errorf("describe %s: %w", endpoint, err)
	}
	r.mu.Lock()
	r.entries[manifest.GetId()] = &Entry{
		Endpoint: endpoint,
		Manifest: manifest,
		client:   client,
	}
	r.mu.Unlock()
	return nil
}

func describeWithRetry(ctx context.Context, client pluginpb.PluginClient) (*pluginpb.PluginManifest, error) {
	const attemptTimeout = 2 * time.Second
	const backoff = 500 * time.Millisecond
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		manifest, err := client.Describe(attemptCtx, &pluginpb.DescribeRequest{})
		cancel()
		if err == nil {
			return manifest, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(backoff):
		}
	}
}

func (r *Registry) Manifests() []*pluginpb.PluginManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*pluginpb.PluginManifest, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.Manifest)
	}
	return out
}

func (r *Registry) Manifest(pluginID string) *pluginpb.PluginManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.entries[pluginID]; ok {
		return e.Manifest
	}
	return nil
}

// ExecuteArgs bundles all parameters for an Execute call.
type ExecuteArgs struct {
	PluginID string
	Action   string
	// Payload is the opaque step params blob.
	Payload []byte
	// Identity passed to the plugin for state correlation.
	InstanceID     string
	InstanceConfig []byte
	WorkflowRunID  string
	WorkflowID     string
	StepID         string
	Attempt        uint32
	IdempotencyKey string
	// UpstreamOutputs maps upstream step IDs to their structured output
	// bags. The plugin resolves its own reference markers against this.
	UpstreamOutputs map[string]map[string][]byte
	// OnProgress, if set, is called for every progress event the plugin
	// emits before its terminal Final event. The payload is the opaque
	// bytes the plugin sent; the callback is responsible for storing /
	// broadcasting it. A slow callback back-pressures the stream.
	OnProgress func(payload []byte)
	// Resolution is the opaque blob the plugin's "resolution" MFE slot
	// produced for a previously-suspended step. Empty on first execution.
	Resolution []byte
}

// ExecuteResult bundles the plugin's response fields.
// Exactly one of (Result/Outputs) or (Suspended/Prompt) is meaningful:
// a plugin ends its stream with either Final or Suspend.
type ExecuteResult struct {
	Result  []byte
	Outputs map[string][]byte
	// Suspended is true when the plugin ended the stream with
	// ExecuteSuspend; Prompt carries its opaque payload for the
	// plugin's "resolution" MFE slot.
	Suspended bool
	Prompt    []byte
}

// Execute invokes a plugin action and returns the opaque result blob
// plus the structured outputs the plugin emitted. Progress events are
// forwarded to args.OnProgress (if set) as they arrive.
func (r *Registry) Execute(ctx context.Context, args ExecuteArgs) (ExecuteResult, error) {
	r.mu.RLock()
	entry, ok := r.entries[args.PluginID]
	r.mu.RUnlock()
	if !ok {
		return ExecuteResult{}, fmt.Errorf("unknown plugin %q", args.PluginID)
	}

	req := &pluginpb.ExecuteRequest{
		Action:          args.Action,
		Payload:         args.Payload,
		InstanceId:      args.InstanceID,
		InstanceConfig:  args.InstanceConfig,
		WorkflowRunId:   args.WorkflowRunID,
		WorkflowId:      args.WorkflowID,
		StepId:          args.StepID,
		Attempt:         args.Attempt,
		IdempotencyKey:  args.IdempotencyKey,
		UpstreamOutputs: marshalUpstream(args.UpstreamOutputs),
		Resolution:      args.Resolution,
	}

	stream, err := entry.client.Execute(ctx, req)
	if err != nil {
		return ExecuteResult{}, err
	}

	var out ExecuteResult
	sawTerminal := false
	for {
		evt, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ExecuteResult{}, err
		}
		switch e := evt.GetEvent().(type) {
		case *pluginpb.ExecuteEvent_Progress:
			if args.OnProgress != nil {
				args.OnProgress(e.Progress.GetPayload())
			}
		case *pluginpb.ExecuteEvent_Final:
			if sawTerminal {
				return ExecuteResult{}, fmt.Errorf("plugin emitted more than one terminal (Final/Suspend) event")
			}
			sawTerminal = true
			out.Result = e.Final.GetResult()
			if outs := e.Final.GetOutputs(); outs != nil {
				out.Outputs = outs.GetValues()
			}
		case *pluginpb.ExecuteEvent_Suspend:
			if sawTerminal {
				return ExecuteResult{}, fmt.Errorf("plugin emitted more than one terminal (Final/Suspend) event")
			}
			sawTerminal = true
			out.Suspended = true
			out.Prompt = e.Suspend.GetPrompt()
		}
	}
	if !sawTerminal {
		return ExecuteResult{}, fmt.Errorf("plugin closed the Execute stream without a Final or Suspend event")
	}
	return out, nil
}

// ValidateArgs bundles params for a Plugin.Validate call.
type ValidateArgs struct {
	PluginID        string
	Kind            string // "instance" | "step"
	Payload         []byte
	InstanceID      string
	InstanceConfig  []byte
	UpstreamOutputs map[string]map[string][]byte
	WorkflowID      string
}

// Validate asks the plugin to vet an opaque blob. Errors are returned
// verbatim to the caller.
func (r *Registry) Validate(ctx context.Context, args ValidateArgs) (bool, []string, error) {
	r.mu.RLock()
	entry, ok := r.entries[args.PluginID]
	r.mu.RUnlock()
	if !ok {
		return false, nil, fmt.Errorf("unknown plugin %q", args.PluginID)
	}
	resp, err := entry.client.Validate(ctx, &pluginpb.ValidateRequest{
		Kind:            args.Kind,
		Payload:         args.Payload,
		InstanceId:      args.InstanceID,
		InstanceConfig:  args.InstanceConfig,
		UpstreamOutputs: marshalUpstream(args.UpstreamOutputs),
		WorkflowId:      args.WorkflowID,
	})
	if err != nil {
		return false, nil, err
	}
	return resp.GetValid(), resp.GetErrors(), nil
}

// ReleaseStepArgs bundles params for a Plugin.ReleaseStep call.
type ReleaseStepArgs struct {
	PluginID      string
	WorkflowRunID string
	StepID        string
	WorkflowID    string
	InstanceID    string
	Action        string
	Status        string
}

// ReleaseStep tells a plugin a step is terminal so it can drop whatever
// it kept for it. A plugin that doesn't implement the RPC has nothing
// to release, so Unimplemented is not an error.
func (r *Registry) ReleaseStep(ctx context.Context, args ReleaseStepArgs) error {
	r.mu.RLock()
	entry, ok := r.entries[args.PluginID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown plugin %q", args.PluginID)
	}
	_, err := entry.client.ReleaseStep(ctx, &pluginpb.ReleaseStepRequest{
		WorkflowRunId: args.WorkflowRunID,
		StepId:        args.StepID,
		WorkflowId:    args.WorkflowID,
		InstanceId:    args.InstanceID,
		Action:        args.Action,
		Status:        args.Status,
	})
	if status.Code(err) == codes.Unimplemented {
		return nil
	}
	return err
}

// PurgeWorkflow asks a plugin to delete everything it keeps for a
// workflow. A plugin that doesn't implement the RPC holds nothing to
// purge, so Unimplemented returns (nil, nil).
func (r *Registry) PurgeWorkflow(ctx context.Context, pluginID, workflowID string) (map[string]int64, error) {
	r.mu.RLock()
	entry, ok := r.entries[pluginID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown plugin %q", pluginID)
	}
	resp, err := entry.client.PurgeWorkflow(ctx, &pluginpb.PurgeWorkflowRequest{WorkflowId: workflowID})
	if status.Code(err) == codes.Unimplemented {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return resp.GetDeleted(), nil
}

// Asset is an open Plugin.GetAsset stream whose header has been read.
// The body is never inspected: WriteTo copies the plugin's bytes as-is.
type Asset struct {
	ContentType string
	ETag        string
	NotModified bool
	stream      grpc.ServerStreamingClient[pluginpb.AssetChunk]
}

// GetAsset asks a plugin for one file of its MFE bundle and reads the
// stream's header. gRPC status errors from the plugin (NotFound,
// InvalidArgument, …) are returned unwrapped so callers can map them.
// The stream lives as long as ctx; drain it with WriteTo.
func (r *Registry) GetAsset(ctx context.Context, pluginID, path, ifNoneMatch string) (*Asset, error) {
	r.mu.RLock()
	entry, ok := r.entries[pluginID]
	r.mu.RUnlock()
	if !ok {
		return nil, ErrUnknownPlugin
	}
	stream, err := entry.client.GetAsset(ctx, &pluginpb.GetAssetRequest{Path: path, IfNoneMatch: ifNoneMatch})
	if err != nil {
		return nil, err
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	h := first.GetHeader()
	if h == nil {
		return nil, fmt.Errorf("plugin %q: GetAsset stream did not start with a header", pluginID)
	}
	return &Asset{
		ContentType: h.GetContentType(),
		ETag:        h.GetEtag(),
		NotModified: h.GetNotModified(),
		stream:      stream,
	}, nil
}

// WriteTo copies the asset's remaining data chunks to w.
func (a *Asset) WriteTo(w io.Writer) (int64, error) {
	var n int64
	for {
		chunk, err := a.stream.Recv()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if chunk.GetHeader() != nil {
			return n, fmt.Errorf("GetAsset stream sent a second header")
		}
		m, err := w.Write(chunk.GetData())
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
}

// ErrUnknownPlugin is returned when no registered plugin has the ID.
var ErrUnknownPlugin = errors.New("unknown plugin")

func marshalUpstream(m map[string]map[string][]byte) map[string]*pluginpb.StepOutputs {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*pluginpb.StepOutputs, len(m))
	for stepID, outs := range m {
		out[stepID] = &pluginpb.StepOutputs{Values: outs}
	}
	return out
}
