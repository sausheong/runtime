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
		{"missing spec id", Config{ListenAddr: ":0", PostgresDSN: "x"}, true},
		{"missing dsn", Config{Spec: hrt.AgentSpec{ID: "a", Model: "m"}, ListenAddr: ":0"}, true},
		{"missing listen addr", Config{Spec: hrt.AgentSpec{ID: "a", Model: "m"}, PostgresDSN: "x"}, true},
		{"ok", Config{Spec: hrt.AgentSpec{ID: "a", Model: "m"}, ListenAddr: ":0", PostgresDSN: "x"}, false},
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
