package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }

func step(id string, deps ...string) Step {
	return Step{ID: id, Plugin: "p", Instance: "i", Action: "run", DependsOn: deps}
}

func TestLoadDirParsesInputs(t *testing.T) {
	dir := t.TempDir()
	yaml := `
id: wf
inputs:
  - name: VERSION
    description: version to build
    default: "1.0"
  - name: TARGET
    required: true
steps:
  - id: a
    plugin: p
    instance: i
    action: run
`
	if err := os.WriteFile(filepath.Join(dir, "wf.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	defs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []InputDecl{
		{Name: "VERSION", Description: "version to build", Default: strp("1.0")},
		{Name: "TARGET", Required: true},
	}
	if !reflect.DeepEqual(defs[0].Inputs, want) {
		t.Fatalf("inputs = %+v, want %+v", defs[0].Inputs, want)
	}
}

func TestValidateInputs(t *testing.T) {
	cases := map[string]struct {
		schedule string
		inputs   []InputDecl
		wantErr  string
	}{
		"ok":                 {inputs: []InputDecl{{Name: "A_1"}, {Name: "B", Required: true}}},
		"lowercase":          {inputs: []InputDecl{{Name: "version"}}, wantErr: "must match"},
		"duplicate":          {inputs: []InputDecl{{Name: "A"}, {Name: "A"}}, wantErr: "duplicate input"},
		"required+default":   {inputs: []InputDecl{{Name: "A", Required: true, Default: strp("x")}}, wantErr: "pick one"},
		"scheduled+required": {schedule: "0 6 * * 1", inputs: []InputDecl{{Name: "A", Required: true}}, wantErr: "scheduled"},
		"scheduled+default":  {schedule: "0 6 * * 1", inputs: []InputDecl{{Name: "A", Default: strp("x")}}},
	}
	for name, c := range cases {
		def := Definition{ID: "wf", Schedule: c.schedule, Inputs: c.inputs, Steps: []Step{step("a")}}
		err := ValidateShape(def)
		if c.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: error = %v, want containing %q", name, err, c.wantErr)
		}
	}
}

func TestResolveInputs(t *testing.T) {
	def := Definition{ID: "wf", Inputs: []InputDecl{
		{Name: "VERSION", Default: strp("1.0")},
		{Name: "TARGET", Required: true},
		{Name: "EXTRA"},
	}}
	got, err := ResolveInputs(def, map[string]string{"TARGET": "all"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"VERSION": "1.0", "TARGET": "all", "EXTRA": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	got, err = ResolveInputs(def, map[string]string{"TARGET": "all", "VERSION": ""})
	if err != nil || got["VERSION"] != "" {
		t.Errorf("explicit empty should override default: %v %v", got, err)
	}
	if _, err := ResolveInputs(def, map[string]string{}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("missing required: err = %v", err)
	}
	if _, err := ResolveInputs(def, map[string]string{"TARGET": "x", "NOPE": "y"}); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("unknown input: err = %v", err)
	}
}

func TestRerunPlan(t *testing.T) {
	// a -> b -> d, a -> c -> d, e independent
	def := Definition{ID: "wf", Steps: []Step{
		step("a"), step("b", "a"), step("c", "a"), step("d", "b", "c"), step("e"),
	}}
	cases := []struct {
		from         string
		rerun, reuse []string
	}{
		{"a", []string{"a", "b", "c", "d"}, []string{"e"}},
		{"b", []string{"b", "d"}, []string{"a", "c", "e"}},
		{"d", []string{"d"}, []string{"a", "b", "c", "e"}},
		{"e", []string{"e"}, []string{"a", "b", "c", "d"}},
	}
	for _, c := range cases {
		rerun, reuse, err := RerunPlan(def, c.from)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rerun, c.rerun) || !reflect.DeepEqual(reuse, c.reuse) {
			t.Errorf("from %s: rerun=%v reuse=%v, want %v %v", c.from, rerun, reuse, c.rerun, c.reuse)
		}
	}
	if _, _, err := RerunPlan(def, "zz"); err == nil {
		t.Error("unknown step: want error")
	}
}
