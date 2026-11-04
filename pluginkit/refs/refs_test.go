package refs

import (
	"strings"
	"testing"

	pluginpb "github.com/jacobsee/upstream-readiness/proto/gen/pluginsdk/v1"
)

func TestResolveRefsAndInputs(t *testing.T) {
	upstream := map[string]*pluginpb.StepOutputs{
		"a": {Values: map[string][]byte{"commit": []byte(`"abc"`)}},
	}
	inputs := map[string]string{"VERSION": "1.2"}
	payload := `{"target":{"__ref":{"step":"a","output":"commit"}},"commands":[{"name":"n","run":{"__input":"VERSION"}}],"keep":{"__input":"VERSION","x":1}}`
	got, err := ResolveRefs([]byte(payload), upstream, inputs)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"commands":[{"name":"n","run":"1.2"}],"keep":{"__input":"VERSION","x":1},"target":"abc"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestResolveInputErrors(t *testing.T) {
	for payload, want := range map[string]string{
		`{"v":{"__input":"NOPE"}}`: `no input for`,
		`{"v":{"__input":""}}`:     `non-empty`,
		`{"v":{"__input":3}}`:      `non-empty`,
	} {
		_, err := ResolveRefs([]byte(payload), nil, map[string]string{"VERSION": "1"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want containing %q", payload, err, want)
		}
	}
}
