package gateway

import (
	"context"

	"github.com/sausheong/runtime/internal/identity"
)

// enrichClaims maps the fixed claim vocabulary to a principal accessor.
var enrichClaims = map[string]func(identity.Principal) string{
	"tenant":  func(p identity.Principal) string { return p.TenantID },
	"subject": func(p identity.Principal) string { return p.Subject },
	"role":    func(p identity.Principal) string { return string(p.Role) },
}

// ResolveEnrichedHeaders turns an upstream's enrich map (claim→header) into
// concrete header→value pairs for this principal. A missing/empty claim value
// omits that header. Returns nil when there is nothing to inject.
func ResolveEnrichedHeaders(enrich map[string]string, p identity.Principal, ok bool) map[string]string {
	if len(enrich) == 0 || !ok {
		return nil
	}
	out := map[string]string{}
	for claim, header := range enrich {
		accessor, known := enrichClaims[claim]
		if !known {
			continue // config validation already rejected unknown claims
		}
		if v := accessor(p); v != "" {
			out[header] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type enrichCtxKey struct{}

// WithEnrichedHeaders attaches per-call enriched headers to ctx (read by the
// REST adapter's Execute).
func WithEnrichedHeaders(ctx context.Context, h map[string]string) context.Context {
	if len(h) == 0 {
		return ctx
	}
	return context.WithValue(ctx, enrichCtxKey{}, h)
}

// EnrichedHeadersFrom returns the enriched headers on ctx, or nil.
func EnrichedHeadersFrom(ctx context.Context) map[string]string {
	h, _ := ctx.Value(enrichCtxKey{}).(map[string]string)
	return h
}
