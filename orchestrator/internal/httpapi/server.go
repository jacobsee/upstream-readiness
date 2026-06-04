// Package httpapi serves the orchestrator's web shell and the JSON
// surface the shell uses to manage plugin instances, workflows, and
// runs. The orchestrator forwards opaque blobs verbatim; it never
// parses instance config, step params, or step outputs.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/registry"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/runbus"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/storage"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/wfengine"
	"github.com/jacobsee/upstream-readiness/orchestrator/internal/workflow"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	reg    *registry.Registry
	web    fs.FS
	mux    *http.ServeMux
	engine *wfengine.Engine
	store  *storage.Store
}

func New(reg *registry.Registry, web fs.FS, engine *wfengine.Engine, store *storage.Store) *Server {
	s := &Server{
		reg:    reg,
		web:    web,
		mux:    http.NewServeMux(),
		engine: engine,
		store:  store,
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.Handle("/", http.FileServer(http.FS(s.web)))
	s.mux.HandleFunc("/api/plugins", s.listPlugins)
	s.mux.HandleFunc("/api/plugins/", s.pluginRouter)
	s.mux.HandleFunc("GET /plugins/{id}/assets/{path...}", s.pluginAsset)
	s.mux.HandleFunc("/api/instances", s.instancesRoot)
	s.mux.HandleFunc("/api/instances/", s.instanceRouter)
	s.mux.HandleFunc("/api/workflows", s.workflowsRoot)
	s.mux.HandleFunc("/api/workflows/", s.workflowRouter)
	s.mux.HandleFunc("/api/runs", s.listAllRuns)
	s.mux.HandleFunc("/api/runs/", s.runRouter)
}

// streamRun serves the SSE stream for a single run. The first frame is
// a "snapshot" event carrying the current summary + step rows so a late
// subscriber doesn't need to re-fetch. Subsequent frames are live
// events published to the run-event bus by the workflow activities.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	summary, err := s.engine.GetRunStatus(r.Context(), runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if summary == nil {
		http.NotFound(w, r)
		return
	}
	bus := s.engine.Bus()
	// Subscribe before writing the snapshot so any event published while
	// we're still writing headers is queued rather than lost.
	var ch <-chan runbus.Event
	if bus != nil {
		var unsub func()
		ch, unsub = bus.Subscribe(runID)
		defer unsub()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	steps, _ := s.store.ListStepsForRun(r.Context(), runID)
	snap, _ := json.Marshal(struct {
		Summary *storage.RunSummary  `json:"summary"`
		Steps   []storage.StepRecord `json:"steps"`
	}{Summary: summary, Steps: steps})
	writeSSE(w, "snapshot", snap)
	flusher.Flush()

	// If the run is already terminal, no live events are coming; close
	// after the snapshot.
	if summary.Status != "running" {
		return
	}

	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-pingTicker.C:
			// Comment line keeps proxies from closing the idle stream.
			_, _ = w.Write([]byte(": keep-alive\n\n"))
			flusher.Flush()
		case evt, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, evt.Type, evt.Data)
			flusher.Flush()
			if evt.Type == "run-status" {
				// Terminal status - no further events; let the client
				// close on the next EOF.
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, event string, data []byte) {
	if event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

// listAllRuns returns recent runs across every workflow, newest first.
// Optional query params: limit (default 50, max 500), status (running |
// succeeded | failed). Powers the dashboard's cross-workflow Runs view.
func (s *Server) listAllRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	status := r.URL.Query().Get("status")
	runs, err := s.store.ListRuns(r.Context(), status, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []storage.RunSummary{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// ── plugin manifest handlers ──────────────────────────────────────────────────

type manifestJSON struct {
	ID          string       `json:"id"`
	DisplayName string       `json:"displayName"`
	Version     string       `json:"version"`
	MfeURL      string       `json:"mfeUrl"`
	MfeElement  string       `json:"mfeElement"`
	Actions     []actionJSON `json:"actions"`
	MfeSlots    []slotJSON   `json:"mfeSlots,omitempty"`
}

type actionJSON struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Outputs     []actionOutputJSON `json:"outputs,omitempty"`
}

type actionOutputJSON struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type slotJSON struct {
	Slot       string `json:"slot"`
	MfeElement string `json:"mfeElement,omitempty"`
}

func toManifestJSON(m *pluginpb.PluginManifest) manifestJSON {
	actions := make([]actionJSON, 0, len(m.GetActions()))
	for _, a := range m.GetActions() {
		aj := actionJSON{Name: a.GetName(), Description: a.GetDescription()}
		for _, o := range a.GetOutputs() {
			aj.Outputs = append(aj.Outputs, actionOutputJSON{
				Name: o.GetName(), Description: o.GetDescription(),
			})
		}
		actions = append(actions, aj)
	}
	slots := make([]slotJSON, 0, len(m.GetMfeSlots()))
	for _, sl := range m.GetMfeSlots() {
		slots = append(slots, slotJSON{Slot: sl.GetSlot(), MfeElement: sl.GetMfeElement()})
	}
	return manifestJSON{
		ID: m.GetId(), DisplayName: m.GetDisplayName(), Version: m.GetVersion(),
		MfeURL: pluginAssetURL(m.GetId(), m.GetMfeEntry()), MfeElement: m.GetMfeElement(),
		Actions: actions, MfeSlots: slots,
	}
}

// pluginAssetURL is the same-origin URL the shell dynamic-imports a
// plugin's MFE entry from. Served by pluginAsset; relative imports inside
// the module resolve under the same /plugins/{id}/assets/ prefix.
func pluginAssetURL(pluginID, assetPath string) string {
	u := url.URL{Path: "/plugins/" + pluginID + "/assets/" + assetPath}
	return u.EscapedPath()
}

// pluginAsset serves one file of a plugin's MFE bundle by streaming the
// plugin's GetAsset RPC through verbatim, so the browser only ever
// talks to the orchestrator. Caching is ETag revalidation: the plugin
// computes the validator, the orchestrator just forwards it.
func (s *Server) pluginAsset(w http.ResponseWriter, r *http.Request) {
	pluginID, assetPath := r.PathValue("id"), r.PathValue("path")
	if !fs.ValidPath(assetPath) || assetPath == "." {
		http.Error(w, "invalid asset path", http.StatusBadRequest)
		return
	}
	asset, err := s.reg.GetAsset(r.Context(), pluginID, assetPath, r.Header.Get("If-None-Match"))
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrUnknownPlugin), status.Code(err) == codes.NotFound:
			http.NotFound(w, r)
		case status.Code(err) == codes.InvalidArgument:
			http.Error(w, status.Convert(err).Message(), http.StatusBadRequest)
		default:
			http.Error(w, fmt.Sprintf("plugin %q: %v", pluginID, err), http.StatusBadGateway)
		}
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	if asset.ETag != "" {
		h.Set("ETag", asset.ETag)
	}
	if asset.NotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", asset.ContentType)
	if _, err := asset.WriteTo(w); err != nil {
		// Headers are already sent; all we can do is cut the response
		// short so the browser sees a failed load rather than a
		// truncated module.
		log.Printf("asset %s/%s: %v", pluginID, assetPath, err)
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) listPlugins(w http.ResponseWriter, _ *http.Request) {
	manifests := s.reg.Manifests()
	out := make([]manifestJSON, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, toManifestJSON(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) pluginRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/plugins/")
	id, verb, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		http.NotFound(w, r)
		return
	}
	switch verb {
	case "validate-instance":
		s.validateBlob(w, r, id, "instance")
	case "validate-step":
		s.validateBlob(w, r, id, "step")
	case "execute":
		s.executeAdHoc(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

type executeReq struct {
	Action     string          `json:"action"`
	Payload    json.RawMessage `json:"payload"`
	InstanceID string          `json:"instance_id"`
	// Optional scoping - plugins that persist per-(workflow,step) memory
	// use these to key their own state when the MFE is mounted from
	// inside the workflow-step-config slot.
	WorkflowID string `json:"workflow_id,omitempty"`
	StepID     string `json:"step_id,omitempty"`
	// WorkflowRunID optionally scopes the call to an existing run so a
	// plugin MFE (step-runtime, resolution) can read its own per-run
	// state. Forwarded verbatim; defaults to "ad-hoc".
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
}

// executeAdHoc forwards a one-shot Execute call from the plugin's own
// action MFE. Ad-hoc invocations are outside any workflow, so there is
// no upstream_outputs, no run_id, and no idempotency_key. The plugin
// gets a stub identity tuple identifying the call as ad-hoc.
func (s *Server) executeAdHoc(w http.ResponseWriter, r *http.Request, pluginID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req executeReq
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.InstanceID == "" {
		http.Error(w, "instance_id is required", http.StatusBadRequest)
		return
	}
	inst, err := s.store.GetInstance(r.Context(), req.InstanceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if inst == nil {
		http.Error(w, fmt.Sprintf("unknown instance %q", req.InstanceID), http.StatusUnprocessableEntity)
		return
	}
	if inst.PluginID != pluginID {
		http.Error(w, fmt.Sprintf("instance %q belongs to plugin %q, not %q", req.InstanceID, inst.PluginID, pluginID), http.StatusUnprocessableEntity)
		return
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}
	stepID := req.StepID
	if stepID == "" {
		stepID = "ad-hoc"
	}
	runID := req.WorkflowRunID
	if runID == "" {
		runID = "ad-hoc"
	}
	result, err := s.reg.Execute(r.Context(), registry.ExecuteArgs{
		PluginID:       pluginID,
		Action:         req.Action,
		Payload:        []byte(req.Payload),
		InstanceID:     inst.ID,
		InstanceConfig: []byte(inst.ConfigBlob),
		WorkflowRunID:  runID,
		WorkflowID:     req.WorkflowID,
		StepID:         stepID,
		Attempt:        1,
		IdempotencyKey: "ad-hoc",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if result.Suspended {
		// Suspension only has meaning inside a workflow run, where the
		// orchestrator can park the step and resume it. Ad-hoc callers
		// have nothing to resume.
		http.Error(w, "plugin suspended the action (awaiting resolution); suspension is only supported inside workflow runs", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Result)
}

type validateReq struct {
	Payload         json.RawMessage                       `json:"payload"`
	InstanceID      string                                `json:"instance_id,omitempty"`
	UpstreamOutputs map[string]map[string]json.RawMessage `json:"upstream_outputs,omitempty"`
}

type validateResp struct {
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors,omitempty"`
}

func (s *Server) validateBlob(w http.ResponseWriter, r *http.Request, pluginID, kind string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req validateReq
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args := registry.ValidateArgs{
		PluginID: pluginID,
		Kind:     kind,
		Payload:  []byte(req.Payload),
	}
	if req.InstanceID != "" {
		inst, err := s.store.GetInstance(r.Context(), req.InstanceID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if inst != nil {
			args.InstanceID = inst.ID
			args.InstanceConfig = []byte(inst.ConfigBlob)
		}
	}
	if len(req.UpstreamOutputs) > 0 {
		args.UpstreamOutputs = convertUpstream(req.UpstreamOutputs)
	}
	valid, errs, err := s.reg.Validate(r.Context(), args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, validateResp{Valid: valid, Errors: errs})
}

// ── instance handlers ─────────────────────────────────────────────────────────

func (s *Server) instancesRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pluginID := r.URL.Query().Get("plugin")
		instances, err := s.store.ListInstances(r.Context(), pluginID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if instances == nil {
			instances = []storage.Instance{}
		}
		writeJSON(w, http.StatusOK, instances)
	case http.MethodPost:
		s.upsertInstance(w, r, "")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) instanceRouter(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/instances/")
	id = path.Clean(id)
	if id == "" || id == "." || id == "/" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		inst, err := s.store.GetInstance(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if inst == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, inst)
	case http.MethodPut:
		s.upsertInstance(w, r, id)
	case http.MethodDelete:
		if err := s.store.DeleteInstance(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type instanceReq struct {
	ID          string          `json:"id,omitempty"`
	PluginID    string          `json:"plugin_id"`
	DisplayName string          `json:"display_name"`
	ConfigBlob  json.RawMessage `json:"config_blob"`
}

func (s *Server) upsertInstance(w http.ResponseWriter, r *http.Request, pathID string) {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req instanceReq
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := req.ID
	if pathID != "" {
		id = pathID
	}
	if id == "" {
		id = uuid.NewString()
	}
	if req.PluginID == "" {
		http.Error(w, "plugin_id is required", http.StatusBadRequest)
		return
	}
	if s.reg.Manifest(req.PluginID) == nil {
		http.Error(w, fmt.Sprintf("unknown plugin %q", req.PluginID), http.StatusUnprocessableEntity)
		return
	}
	if len(req.ConfigBlob) == 0 {
		req.ConfigBlob = json.RawMessage(`{}`)
	}
	valid, vErrs, err := s.reg.Validate(r.Context(), registry.ValidateArgs{
		PluginID: req.PluginID,
		Kind:     "instance",
		Payload:  []byte(req.ConfigBlob),
	})
	if err == nil && !valid {
		http.Error(w, "instance config invalid: "+strings.Join(vErrs, "; "), http.StatusUnprocessableEntity)
		return
	}
	inst := storage.Instance{
		ID: id, PluginID: req.PluginID, DisplayName: req.DisplayName, ConfigBlob: req.ConfigBlob,
	}
	if err := s.store.UpsertInstance(r.Context(), inst); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	saved, err := s.store.GetInstance(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pathID == "" {
		writeJSON(w, http.StatusCreated, saved)
	} else {
		writeJSON(w, http.StatusOK, saved)
	}
}

// ── workflow handlers ─────────────────────────────────────────────────────────

// workflowJSON is a definition plus the orchestrator's bookkeeping about
// it. Workflows are defined only by the YAML files in --workflows; there
// is no API to create or edit them.
type workflowJSON struct {
	workflow.Definition
	// RemovedAt is set when the workflow's file was gone at startup.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}

func (s *Server) workflowsRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	recs, err := s.store.ListWorkflows(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]workflowJSON, 0, len(recs))
	for _, rec := range recs {
		def, err := decodeDefinition(rec)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, workflowJSON{Definition: def, RemovedAt: rec.RemovedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) workflowRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	id, rest2, hasVerb := strings.Cut(rest, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	rec, err := s.store.GetWorkflow(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	def, err := decodeDefinition(*rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !hasVerb {
		writeJSON(w, http.StatusOK, workflowJSON{Definition: def, RemovedAt: rec.RemovedAt})
		return
	}
	verb, _, _ := strings.Cut(rest2, "/")
	switch verb {
	case "run":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if rec.RemovedAt != nil {
			http.Error(w, fmt.Sprintf("workflow %q was removed (its file is no longer in the workflows directory)", id), http.StatusConflict)
			return
		}
		runID, err := s.engine.TriggerRun(r.Context(), def)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
	case "runs":
		runs, err := s.store.ListRunsForWorkflow(r.Context(), id, 50)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	case "purge":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		s.purgeWorkflow(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// purgeWorkflow deletes every trace of a removed workflow (see
// wfengine.PurgeWorkflow). The response is the PurgeReport; on a plugin
// failure it is returned with 502 so the UI can show which plugin failed.
func (s *Server) purgeWorkflow(w http.ResponseWriter, r *http.Request, id string) {
	report, err := s.engine.PurgeWorkflow(r.Context(), id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, report)
	case errors.Is(err, storage.ErrWorkflowNotRemoved):
		http.Error(w, fmt.Sprintf("workflow %q is not removed; only workflows whose file is gone can be purged", id), http.StatusConflict)
	case errors.Is(err, wfengine.ErrPurgeBlocked):
		http.Error(w, err.Error(), http.StatusConflict)
	case report != nil && len(report.PluginErrors) > 0:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "report": report})
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ── run handlers ──────────────────────────────────────────────────────────────

func (s *Server) runRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	runID, sub, _ := strings.Cut(rest, "/")
	if runID == "" {
		http.NotFound(w, r)
		return
	}
	// verb is the first segment after the run id; sub keeps the rest
	// for nested routes (steps/:step_id/resolve).
	verb, sub, _ := strings.Cut(sub, "/")
	switch verb {
	case "":
		summary, err := s.engine.GetRunStatus(r.Context(), runID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if summary == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, summary)
	case "events":
		s.streamRun(w, r, runID)
	case "cancel":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if err := s.engine.CancelRun(r.Context(), runID); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "steps":
		// /api/runs/:run_id/steps/:step_id/resolve
		stepID, stepVerb, _ := strings.Cut(sub, "/")
		if stepID == "" || stepVerb != "resolve" {
			http.NotFound(w, r)
			return
		}
		s.resolveStep(w, r, runID, stepID)
	default:
		http.NotFound(w, r)
	}
}

type resolveReq struct {
	// Resolution is the opaque blob the plugin's "resolution" MFE slot
	// produced. Forwarded verbatim to the plugin on re-execution.
	Resolution json.RawMessage `json:"resolution"`
}

// resolveStep delivers a resolution to a step parked in
// awaiting_resolution by signalling its Temporal workflow.
func (s *Server) resolveStep(w http.ResponseWriter, r *http.Request, runID, stepID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req resolveReq
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.engine.ResolveStep(r.Context(), runID, stepID, req.Resolution); err != nil {
		if errors.Is(err, wfengine.ErrStepNotAwaiting) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func decodeDefinition(rec storage.WorkflowRecord) (workflow.Definition, error) {
	var def workflow.Definition
	if err := json.Unmarshal([]byte(rec.Definition), &def); err != nil {
		return def, fmt.Errorf("decode workflow %q: %w", rec.ID, err)
	}
	return def, nil
}

func convertUpstream(in map[string]map[string]json.RawMessage) map[string]map[string][]byte {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string][]byte, len(in))
	for k, m := range in {
		sub := make(map[string][]byte, len(m))
		for k2, v := range m {
			sub[k2] = []byte(v)
		}
		out[k] = sub
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
