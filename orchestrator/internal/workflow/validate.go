package workflow

import (
	"fmt"
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
