package agentruntime

import (
	"testing"

	hrt "github.com/sausheong/harness/runtime"
)

func TestConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"missing spec id", Config{Spec: hrt.AgentSpec{Model: "m"}}, true},
		{"missing model", Config{Spec: hrt.AgentSpec{ID: "a"}}, true},
		{"ok", Config{Spec: hrt.AgentSpec{ID: "a", Model: "m"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}
