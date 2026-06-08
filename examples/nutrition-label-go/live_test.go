//go:build live

package nutrition

import (
	"context"
	"os"
	"testing"

	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"
)

// TestLiveOneTurn drives a single real turn against the configured proxy.
// Skips unless OPENAI_API_KEY is set. Run:
//
//	OPENAI_API_KEY=... OPENAI_BASE_URL=https://litellm-stg.aip.gov.sg OPENAI_MODEL=gpt-5.4 \
//	  go test -tags live ./examples/nutrition-label-go/ -run TestLiveOneTurn -v
func TestLiveOneTurn(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY not set; skipping live smoke")
	}
	cfg, err := BuildConfig(Deps{AgentID: "nutrition"})
	if err != nil {
		t.Fatal(err)
	}

	sess := session.NewSession("nutrition", "live-1")
	rt, err := hrt.BuildRuntime(hrt.RuntimeDeps{}, hrt.RuntimeInputs{
		Provider: cfg.Provider, Tools: cfg.Tools, Session: sess,
	}, cfg.Spec)
	if err != nil {
		t.Fatal(err)
	}

	msg := "Investigate this label (text): Product: Test Soda. Ingredients: water, sugar, E211, soy lecithin. Sugar 11g/100ml, sat fat 0g/100ml. It is a beverage."
	res, err := rt.RunTurn(context.Background(), msg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn done=%v reason=%s entries=%d", res.Done, res.StopReason, len(res.Entries))
	if res.StopReason == "error" {
		t.Fatalf("turn errored: %v", res.Err)
	}
}
