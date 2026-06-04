package workflow

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDirKeepsStepTimeout(t *testing.T) {
	dir := t.TempDir()
	yaml := `
id: wf
display_name: WF
steps:
  - id: a
    plugin: p
    instance: i
    action: run
    timeout: 4h
    params: {x: 1}
  - id: b
    plugin: p
    instance: i
    action: run
    depends_on: [a]
`
	if err := os.WriteFile(filepath.Join(dir, "wf.workflow.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	defs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || len(defs[0].Steps) != 2 {
		t.Fatalf("got %+v", defs)
	}
	a, b := defs[0].Steps[0], defs[0].Steps[1]
	if a.Timeout != "4h" || a.StepTimeout() != 4*time.Hour {
		t.Errorf("step a timeout = %q (%v), want 4h", a.Timeout, a.StepTimeout())
	}
	if b.Timeout != "" || b.StepTimeout() != DefaultStepTimeout {
		t.Errorf("step b timeout = %q (%v), want default", b.Timeout, b.StepTimeout())
	}
	if string(a.Params) != `{"x":1}` {
		t.Errorf("step a params = %s", a.Params)
	}
}
