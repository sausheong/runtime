package nutrition

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sausheong/harness/tool"
)

// hcsResourceID is the data.gov.sg resource for the HPB Healthier Choice
// Symbol product list. Copied from the Python HCS_RESOURCE_ID constant.
const hcsResourceID = "d_6725eed000bf5b3c5d310eb08de0851f"

// httpDoer is the minimal HTTP surface the HCS tool needs. The real client is
// http.DefaultClient; tests inject a stub.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// tools bundles the dependencies the four agent tools share: the SFA additive
// index, cross-run memory, and an HTTP client for the HCS lookup.
type tools struct {
	idx  *additiveIndex
	mem  *memory
	http httpDoer
}

// newTools wires the tools together. A nil httpDoer defaults to
// http.DefaultClient.
func newTools(idx *additiveIndex, mem *memory, h httpDoer) *tools {
	if h == nil {
		h = http.DefaultClient
	}
	return &tools{idx: idx, mem: mem, http: h}
}

// toolImpl is a generic tool.Tool adapter so each of the four tools can be a
// small configured value rather than its own type.
type toolImpl struct {
	name, desc string
	params     json.RawMessage
	safe       bool
	exec       func(ctx context.Context, input json.RawMessage) (tool.ToolResult, error)
}

func (t *toolImpl) Name() string                           { return t.name }
func (t *toolImpl) Description() string                    { return t.desc }
func (t *toolImpl) Parameters() json.RawMessage            { return t.params }
func (t *toolImpl) IsConcurrencySafe(json.RawMessage) bool { return t.safe }
func (t *toolImpl) Execute(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	return t.exec(ctx, input)
}

// baseNumber lowercases an additive number and strips any parenthetical
// suffix, e.g. "500(ii)" -> "500". Mirrors the Python re.sub(r"\(.*\)", "", num).lower().
func baseNumber(s string) string {
	return strings.ToLower(reParenAny.ReplaceAllString(s, ""))
}

// resolveWithMemory resolves an additive, first consulting the learned-alias
// memory, then the index, then the optional hint.
func (t *tools) resolveWithMemory(additive, hint string) *additive {
	if num := t.mem.learnedAlias(norm(additive)); num != "" {
		if e := t.idx.resolve(num, ""); e != nil {
			return e
		}
	}
	if e := t.idx.resolve(additive, ""); e != nil {
		return e
	}
	if hint != "" {
		return t.idx.resolve(hint, "")
	}
	return nil
}

// checkAdditive returns the check_sfa_additive tool. Not concurrency-safe: it
// may persist a learned alias.
func (t *tools) checkAdditive() tool.Tool {
	const params = `{"type":"object","properties":{` +
		`"additive":{"type":"string","description":"An E-number ('E211', 'e211', 'en:e211') OR an additive name ('Sodium Benzoate', 'Soy Lecithin', 'MSG')."},` +
		`"e_number_hint":{"type":"string","description":"Optional. If you pass a name and also know its E/INS number, supply it here. If the name isn't recognised but the number is, the mapping is REMEMBERED so the name resolves directly next time."}` +
		`},"required":["additive"]}`
	return &toolImpl{
		name: "check_sfa_additive",
		desc: "Check whether a food additive is permitted by the Singapore Food Agency. " +
			"Looks the additive up in the full SFA permitted-additives list (parsed from " +
			"the official SFA PDF). Accepts either an E-number or a plain-English name as " +
			"printed on a Singapore label.",
		params: json.RawMessage(params),
		safe:   false,
		exec: func(_ context.Context, input json.RawMessage) (tool.ToolResult, error) {
			var args struct {
				Additive    string `json:"additive"`
				ENumberHint string `json:"e_number_hint"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.ToolResult{Error: "invalid input: " + err.Error()}, nil
			}
			raw := strings.TrimSpace(args.Additive)
			hint := strings.TrimSpace(args.ENumberHint)
			entry := t.resolveWithMemory(raw, "")
			if entry == nil && hint != "" {
				entry = t.idx.resolve(hint, "")
				if entry != nil { // learn: this label name -> this number, persisted to disk
					num := entry.ENumber
					if num == "" {
						num = entry.INS
					}
					t.mem.learnAlias(norm(raw), baseNumber(num))
				}
			}
			return tool.ToolResult{Output: t.idx.format(entry, raw)}, nil
		},
	}
}

// recallProduct returns the recall_product tool. Concurrency-safe (read-only).
func (t *tools) recallProduct() tool.Tool {
	const params = `{"type":"object","properties":{` +
		`"product_name":{"type":"string","description":"The product/brand name read off the label."}` +
		`},"required":["product_name"]}`
	return &toolImpl{
		name: "recall_product",
		desc: "Recall whether this product has been investigated before, and the prior verdict. " +
			"Call this FIRST. If a prior record exists, use it to inform (not replace) your " +
			"fresh investigation.",
		params: json.RawMessage(params),
		safe:   true,
		exec: func(_ context.Context, input json.RawMessage) (tool.ToolResult, error) {
			var args struct {
				ProductName string `json:"product_name"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.ToolResult{Error: "invalid input: " + err.Error()}, nil
			}
			rec, ok := t.mem.recall(args.ProductName)
			if !ok {
				return tool.ToolResult{Output: fmt.Sprintf(
					"No prior record of '%s'. This is a first investigation.", args.ProductName)}, nil
			}
			return tool.ToolResult{Output: fmt.Sprintf(
				"Seen before — prior verdict for '%s': %s Recommendation: %s",
				rec.ProductName, rec.Summary, rec.Recommendation)}, nil
		},
	}
}

// checkHCS returns the check_hcs tool. Concurrency-safe (read-only network query).
func (t *tools) checkHCS() tool.Tool {
	const params = `{"type":"object","properties":{` +
		`"product_name":{"type":"string","description":"Brand and product name to search for."}` +
		`},"required":["product_name"]}`
	return &toolImpl{
		name: "check_hcs",
		desc: "Check if a product carries Singapore's Healthier Choice Symbol (HCS). " +
			"Queries the HPB dataset on data.gov.sg.",
		params: json.RawMessage(params),
		safe:   true,
		exec: func(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
			var args struct {
				ProductName string `json:"product_name"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.ToolResult{Error: "invalid input: " + err.Error()}, nil
			}
			out, err := t.queryHCS(ctx, args.ProductName)
			if err != nil {
				// Never break the agent turn: report the failure as output.
				return tool.ToolResult{Output: "HCS check failed (network error): " + err.Error()}, nil
			}
			return tool.ToolResult{Output: out}, nil
		},
	}
}

// queryHCS performs the data.gov.sg lookup and formats the result.
func (t *tools) queryHCS(ctx context.Context, productName string) (string, error) {
	q := url.Values{}
	q.Set("resource_id", hcsResourceID)
	q.Set("q", productName)
	q.Set("limit", "5")
	u := "https://data.gov.sg/api/action/datastore_search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var parsed struct {
		Result struct {
			Records []struct {
				BrandAndProductName string `json:"brand_and_product_name"`
			} `json:"records"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	records := parsed.Result.Records
	if len(records) > 0 {
		matches := make([]string, len(records))
		for i, r := range records {
			name := r.BrandAndProductName
			if name == "" {
				name = "?"
			}
			matches[i] = name
		}
		return "HCS CERTIFIED. Matching products: " + strings.Join(matches, ", "), nil
	}
	return fmt.Sprintf("NOT FOUND in HCS database. '%s' does not appear to carry "+
		"the Healthier Choice Symbol (absence is not necessarily a concern).", productName), nil
}

// nutriGrade returns the calculate_nutri_grade tool. Concurrency-safe (pure).
func (t *tools) nutriGrade() tool.Tool {
	const params = `{"type":"object","properties":{` +
		`"sugar_per_100ml":{"type":"number","description":"Sugar content in grams per 100ml."},` +
		`"saturated_fat_per_100ml":{"type":"number","description":"Saturated fat content in grams per 100ml."}` +
		`},"required":["sugar_per_100ml","saturated_fat_per_100ml"]}`
	return &toolImpl{
		name: "calculate_nutri_grade",
		desc: "Calculate the Singapore Nutri-Grade (A/B/C/D) for a beverage. " +
			"Only call this for beverages, using values read from the nutrition panel.",
		params: json.RawMessage(params),
		safe:   true,
		exec: func(_ context.Context, input json.RawMessage) (tool.ToolResult, error) {
			var args struct {
				Sugar  float64 `json:"sugar_per_100ml"`
				SatFat float64 `json:"saturated_fat_per_100ml"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.ToolResult{Error: "invalid input: " + err.Error()}, nil
			}
			sugar, satFat := args.Sugar, args.SatFat
			var grade, desc string
			switch {
			case sugar <= 1 && satFat <= 0.7:
				grade, desc = "A", "Healthiest tier — very low sugar and saturated fat"
			case sugar <= 5 && satFat <= 1.2:
				grade, desc = "B", "Acceptable — moderate sugar and saturated fat"
			case sugar <= 10 && satFat <= 2.8:
				grade, desc = "C", "Less healthy — must display Nutri-Grade label"
			default:
				grade, desc = "D", "Least healthy — mandatory label; cannot advertise to children"
			}
			out := fmt.Sprintf("Nutri-Grade: %s | Sugar %gg/100ml | Sat fat %gg/100ml | %s",
				grade, sugar, satFat, desc)
			return tool.ToolResult{Output: out}, nil
		},
	}
}
