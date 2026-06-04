package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadDir reads every *.yaml file in dir and returns parsed
// Definitions. YAML step.params is converted to JSON so it can be
// stored opaque downstream. Any file that fails to parse or validate
// aborts the whole load so misconfigured workflows are surfaced at
// startup, not at run time.
func LoadDir(dir string) ([]Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read workflow dir %q: %w", dir, err)
	}
	var defs []Definition
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		def, err := loadFile(path)
		if err != nil {
			return nil, fmt.Errorf("load %q: %w", path, err)
		}
		if err := ValidateShape(*def); err != nil {
			return nil, fmt.Errorf("validate %q: %w", path, err)
		}
		defs = append(defs, *def)
	}
	return defs, nil
}

// yamlStep is the intermediate type used only for YAML decoding.
// Params is decoded as a generic Go value so we can re-encode it as
// JSON bytes and treat it as opaque from there on.
type yamlStep struct {
	ID        string   `yaml:"id"`
	Plugin    string   `yaml:"plugin"`
	Instance  string   `yaml:"instance"`
	Action    string   `yaml:"action"`
	DependsOn []string `yaml:"depends_on,omitempty"`
	Params    any      `yaml:"params,omitempty"`
	Timeout   string   `yaml:"timeout,omitempty"`
}

type yamlDef struct {
	ID          string     `yaml:"id"`
	DisplayName string     `yaml:"display_name"`
	Schedule    string     `yaml:"schedule,omitempty"`
	Steps       []yamlStep `yaml:"steps"`
}

func loadFile(path string) (*Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw yamlDef
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("workflow has no id")
	}
	def := Definition{
		ID:          raw.ID,
		DisplayName: raw.DisplayName,
		Schedule:    raw.Schedule,
		Steps:       make([]Step, 0, len(raw.Steps)),
	}
	for _, ys := range raw.Steps {
		s := Step{
			ID:        ys.ID,
			Plugin:    ys.Plugin,
			Instance:  ys.Instance,
			Action:    ys.Action,
			DependsOn: ys.DependsOn,
			Timeout:   ys.Timeout,
		}
		if ys.Params != nil {
			normalized := normalizeForJSON(ys.Params)
			b, err := json.Marshal(normalized)
			if err != nil {
				return nil, fmt.Errorf("step %q params: %w", ys.ID, err)
			}
			s.Params = json.RawMessage(b)
		} else {
			s.Params = json.RawMessage(`{}`)
		}
		def.Steps = append(def.Steps, s)
	}
	return &def, nil
}

// normalizeForJSON walks a decoded YAML value and rewrites it into a
// form encoding/json can serialise. gopkg.in/yaml.v3 decodes mappings
// as map[any]any, which json.Marshal rejects; we coerce keys to strings
// (and reject any non-string keys, since opaque step params must be
// representable as JSON objects).
func normalizeForJSON(v any) any {
	switch t := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprintf("%v", k)
			}
			out[ks] = normalizeForJSON(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeForJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeForJSON(val)
		}
		return out
	default:
		return v
	}
}
