package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ValidateShape checks the structural invariants of a workflow that the
// orchestrator can verify without talking to any plugin: unique step
// IDs, valid depends_on references, no cycles, every step naming a
// plugin and instance and action. Plugin-domain validation of step
// params is delegated to Plugin.Validate calls performed elsewhere.
func ValidateShape(def Definition) error {
	if def.ID == "" {
		return fmt.Errorf("workflow has no id")
	}
	if len(def.Steps) == 0 {
		return fmt.Errorf("workflow %q has no steps", def.ID)
	}
	if err := validateInputs(def); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, s := range def.Steps {
		if s.ID == "" {
			return fmt.Errorf("step with plugin %q has no id", s.Plugin)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
		if s.Plugin == "" {
			return fmt.Errorf("step %q has no plugin", s.ID)
		}
		if s.Instance == "" {
			return fmt.Errorf("step %q has no instance", s.ID)
		}
		if s.Action == "" {
			return fmt.Errorf("step %q has no action", s.ID)
		}
		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil {
				return fmt.Errorf("step %q timeout %q: %w", s.ID, s.Timeout, err)
			}
			if d <= 0 {
				return fmt.Errorf("step %q timeout must be positive", s.ID)
			}
		}
	}
	for _, s := range def.Steps {
		for _, dep := range s.DependsOn {
			if !seen[dep] {
				return fmt.Errorf("step %q depends_on unknown step %q", s.ID, dep)
			}
			if dep == s.ID {
				return fmt.Errorf("step %q depends_on itself", s.ID)
			}
		}
	}
	return detectCycles(def)
}

var inputNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func validateInputs(def Definition) error {
	seen := map[string]bool{}
	for _, in := range def.Inputs {
		if !inputNameRE.MatchString(in.Name) {
			return fmt.Errorf("input name %q must match %s", in.Name, inputNameRE)
		}
		if seen[in.Name] {
			return fmt.Errorf("duplicate input %q", in.Name)
		}
		seen[in.Name] = true
		if in.Required && in.Default != nil {
			return fmt.Errorf("input %q is required and has a default; pick one", in.Name)
		}
		if in.Required && def.Schedule != "" {
			return fmt.Errorf("input %q is required, but scheduled runs can only use defaults", in.Name)
		}
	}
	return nil
}

// ResolveInputs checks the inputs supplied for one run against the
// workflow's declarations and returns the full set the run carries:
// unknown names are rejected, required inputs must be non-empty, and
// omitted inputs take their default (or "").
func ResolveInputs(def Definition, supplied map[string]string) (map[string]string, error) {
	declared := map[string]bool{}
	for _, in := range def.Inputs {
		declared[in.Name] = true
	}
	var unknown []string
	for k := range supplied {
		if !declared[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("workflow %q declares no input %s", def.ID, strings.Join(unknown, ", "))
	}
	out := make(map[string]string, len(def.Inputs))
	for _, in := range def.Inputs {
		v, ok := supplied[in.Name]
		if !ok && in.Default != nil {
			v = *in.Default
		}
		if in.Required && v == "" {
			return nil, fmt.Errorf("input %q is required", in.Name)
		}
		out[in.Name] = v
	}
	return out, nil
}

// RerunPlan splits def's steps for a rerun starting at fromStep: rerun
// is fromStep plus everything downstream of it, reuse is every other
// step. Both lists keep declaration order.
func RerunPlan(def Definition, fromStep string) (rerun, reuse []string, err error) {
	children := map[string][]string{}
	found := false
	for _, s := range def.Steps {
		if s.ID == fromStep {
			found = true
		}
		for _, dep := range s.DependsOn {
			children[dep] = append(children[dep], s.ID)
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("workflow %q has no step %q", def.ID, fromStep)
	}
	downstream := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if downstream[id] {
			return
		}
		downstream[id] = true
		for _, c := range children[id] {
			walk(c)
		}
	}
	walk(fromStep)
	for _, s := range def.Steps {
		if downstream[s.ID] {
			rerun = append(rerun, s.ID)
		} else {
			reuse = append(reuse, s.ID)
		}
	}
	return rerun, reuse, nil
}

func detectCycles(def Definition) error {
	adj := map[string][]string{}
	for _, s := range def.Steps {
		adj[s.ID] = s.DependsOn
	}
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("cycle detected involving step %q", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dep := range adj[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range adj {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
