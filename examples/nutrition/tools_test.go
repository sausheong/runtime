package nutrition

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func newTestTools(t *testing.T) *tools {
	t.Helper()
	return newTools(newAdditiveIndex(), newMemory(filepath.Join(t.TempDir(), "m.json")), nil)
}

func TestNutriGradeBands(t *testing.T) {
	tl := newTestTools(t)
	cases := []struct {
		sugar, sat float64
		want       string
	}{
		{0.5, 0.5, "A"}, {3, 1.0, "B"}, {8, 2.0, "C"}, {15, 5, "D"},
	}
	for _, c := range cases {
		in, _ := json.Marshal(map[string]float64{"sugar_per_100ml": c.sugar, "saturated_fat_per_100ml": c.sat})
		res, err := tl.nutriGrade().Execute(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output, "Nutri-Grade: "+c.want) {
			t.Errorf("sugar=%v sat=%v: want grade %s, got %q", c.sugar, c.sat, c.want, res.Output)
		}
	}
}

func TestCheckAdditiveLearnsFromHint(t *testing.T) {
	idx := newAdditiveIndex()
	mem := newMemory(filepath.Join(t.TempDir(), "m.json"))
	tl := newTools(idx, mem, nil)
	in, _ := json.Marshal(map[string]string{"additive": "frobnicate gum", "e_number_hint": "415"})
	res, _ := tl.checkAdditive().Execute(context.Background(), in)
	if !strings.Contains(strings.ToLower(res.Output), "permitted") {
		t.Fatalf("hint did not resolve: %q", res.Output)
	}
	if mem.learnedAlias(norm("frobnicate gum")) == "" {
		t.Error("alias was not learned from hint")
	}
}

func TestRecallProductTool(t *testing.T) {
	tl := newTestTools(t)
	in, _ := json.Marshal(map[string]string{"product_name": "Nothing"})
	res, _ := tl.recallProduct().Execute(context.Background(), in)
	if !strings.Contains(res.Output, "first investigation") {
		t.Errorf("want first-investigation, got %q", res.Output)
	}
}

// fakeDoer is a stub httpDoer: it returns a canned body, or an error if set.
type fakeDoer struct {
	body string
	err  error
}

func (f fakeDoer) Do(*http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func TestCheckHCSStubbed(t *testing.T) {
	idx := newAdditiveIndex()
	mem := newMemory(filepath.Join(t.TempDir(), "m.json"))

	// Success path: canned JSON with a matching record.
	ok := newTools(idx, mem, fakeDoer{body: `{"result":{"records":[{"brand_and_product_name":"Milo UHT"}]}}`})
	in, _ := json.Marshal(map[string]string{"product_name": "Milo"})
	res, err := ok.checkHCS().Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Output, "HCS CERTIFIED") || !strings.Contains(res.Output, "Milo UHT") {
		t.Errorf("want HCS CERTIFIED with Milo UHT, got %q", res.Output)
	}

	// Failure path: doer returns an error -> graceful message, nil error.
	bad := newTools(idx, mem, fakeDoer{err: fmt.Errorf("boom")})
	res2, err2 := bad.checkHCS().Execute(context.Background(), in)
	if err2 != nil {
		t.Fatalf("Execute must not return an error on network failure, got %v", err2)
	}
	if !strings.Contains(res2.Output, "HCS check failed") {
		t.Errorf("want graceful HCS failure, got %q", res2.Output)
	}
}
