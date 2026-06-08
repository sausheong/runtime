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
// A zero status defaults to 200.
type fakeDoer struct {
	body   string
	status int
	err    error
}

func (f fakeDoer) Do(*http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
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

	// Non-200 path: mirrors Python's _safe_json returning {} for any non-200
	// response. A 502 with a non-JSON body must degrade to NOT FOUND, never a
	// parse/network failure.
	gw := newTools(idx, mem, fakeDoer{status: 502, body: "<html>bad gateway</html>"})
	res3, err3 := gw.checkHCS().Execute(context.Background(), in)
	if err3 != nil {
		t.Fatalf("Execute must not return an error on non-200, got %v", err3)
	}
	if !strings.Contains(res3.Output, "NOT FOUND") {
		t.Errorf("want NOT FOUND on non-200, got %q", res3.Output)
	}
	if strings.Contains(res3.Output, "HCS check failed") {
		t.Errorf("non-200 must not report a check failure, got %q", res3.Output)
	}
}
