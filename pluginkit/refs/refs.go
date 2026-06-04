// Package refs resolves the {"__ref": {"step": "...", "output": "..."}}
// marker objects the repo's plugins accept anywhere in their step
// params, against the upstream_outputs the orchestrator threads in. The
// orchestrator never parses any of this; the plugin does the
// substitution on Execute.
package refs

import (
	"encoding/json"
	"fmt"

	"github.com/jacobsee/upstream-readiness/pluginkit/gitenv"
	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

// ResolveRefs walks the opaque params blob and replaces every
// {"__ref": {"step": "...", "output": "..."}} object with the JSON value
// the named upstream output produced. Missing references are an error.
func ResolveRefs(payload []byte, upstream map[string]*pluginpb.StepOutputs) ([]byte, error) {
	if len(payload) == 0 {
		return []byte(`{}`), nil
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, err
	}
	resolved, err := resolveValue(v, upstream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(resolved)
}

func resolveValue(v any, upstream map[string]*pluginpb.StepOutputs) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["__ref"]; ok && len(t) == 1 {
			return resolveRef(ref, upstream)
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			r, err := resolveValue(val, upstream)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			r, err := resolveValue(val, upstream)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

func resolveRef(ref any, upstream map[string]*pluginpb.StepOutputs) (any, error) {
	m, ok := ref.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("__ref must be an object")
	}
	step, _ := m["step"].(string)
	output, _ := m["output"].(string)
	if step == "" || output == "" {
		return nil, fmt.Errorf("__ref requires non-empty step and output")
	}
	outs, ok := upstream[step]
	if !ok {
		return nil, fmt.Errorf("__ref targets step %q which is not an upstream of this step", step)
	}
	raw, ok := outs.GetValues()[output]
	if !ok {
		return nil, fmt.Errorf("__ref targets output %q of step %q which the upstream did not emit", output, step)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode upstream output %s.%s: %w", step, output, err)
	}
	return decoded, nil
}

// AutoReplaceDirective describes a `replace` line the plugin will add
// to the cloned repo's go.mod so `go mod tidy` can find the upstream
// commit on the *fork* (where it actually exists) rather than on the
// canonical module path (where it doesn't, until the corresponding PR
// merges).
type AutoReplaceDirective struct {
	// Dependency being bumped - the LHS of the replace directive.
	Dependency string
	// ForkURL is the raw fork URL emitted by the upstream step
	// (e.g. git@github.com:acme-bot/lib-core.git). Retained mainly for
	// commit-message and error output.
	ForkURL string
	// ForkModulePath is the fork rewritten into a Go module path
	// (e.g. github.com/acme-bot/lib-core). Used verbatim as the RHS
	// module in the replace directive.
	ForkModulePath string
	// ForkCommit is the SHA to pin the replace target at.
	ForkCommit string
	// UpstreamStepID is which step's outputs the info came from.
	// Included in the commit message so reviewers can trace the chain.
	UpstreamStepID string
}

// DetectAutoReplace inspects the *unresolved* update entry for a
// target_version that binds to an upstream step, and if that step
// emits both fork_url and commit outputs, returns the directive we'll
// add to go.mod at update time.
//
// Returns (nil, nil) when no auto-replace is warranted. Errors are
// reserved for cases where the shape is obviously broken (referenced
// step exists but outputs are unparseable) - in which case the caller
// should fail loudly rather than silently skip.
func DetectAutoReplace(raw map[string]any, upstream map[string]*pluginpb.StepOutputs, dependency string) (*AutoReplaceDirective, error) {
	if raw == nil {
		return nil, nil
	}
	tv, ok := raw["target_version"].(map[string]any)
	if !ok {
		return nil, nil
	}
	refObj, ok := tv["__ref"].(map[string]any)
	if !ok {
		return nil, nil
	}
	step, _ := refObj["step"].(string)
	if step == "" {
		return nil, nil
	}
	outs, ok := upstream[step]
	if !ok {
		return nil, nil
	}
	values := outs.GetValues()
	forkRaw, hasForkURL := values["fork_url"]
	commitRaw, hasCommit := values["commit"]
	if !hasForkURL || !hasCommit {
		// The upstream step didn't publish both a fork_url and a commit,
		// so we assume the workflow author knows the version will
		// resolve from the canonical module path.
		return nil, nil
	}
	var forkURL, commit string
	if err := json.Unmarshal(forkRaw, &forkURL); err != nil {
		return nil, fmt.Errorf("decode upstream step %q fork_url: %w", step, err)
	}
	if err := json.Unmarshal(commitRaw, &commit); err != nil {
		return nil, fmt.Errorf("decode upstream step %q commit: %w", step, err)
	}
	if forkURL == "" || commit == "" {
		return nil, nil
	}
	modulePath, err := gitenv.GitURLToModulePath(forkURL)
	if err != nil {
		return nil, fmt.Errorf("translate fork URL from step %q to module path: %w", step, err)
	}
	return &AutoReplaceDirective{
		Dependency:     dependency,
		ForkURL:        forkURL,
		ForkModulePath: modulePath,
		ForkCommit:     commit,
		UpstreamStepID: step,
	}, nil
}
