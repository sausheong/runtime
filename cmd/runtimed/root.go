package main

import (
	"net/http"

	"github.com/sausheong/runtime/console"
	"github.com/sausheong/runtime/controlplane"
	"github.com/sausheong/runtime/internal/gateway"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

// rootOptions names the independently optional services mounted on the HTTP
// root. Keeping this as an options object prevents startup wiring from becoming
// an error-prone positional argument list as the platform grows.
type rootOptions struct {
	Registry       *controlplane.Registry
	AdminStore     controlplane.AdminStore
	ConsoleOIDC    console.OIDCConfig
	SecretAdmin    controlplane.SecretAdmin
	Gateway        *gateway.Handler
	UpstreamStore  controlplane.UpstreamStore
	GatewayMutator controlplane.GatewayMutator
	AgentStore     controlplane.AgentStore
	AgentManager   *controlplane.AgentManager
	Onboarding     *console.Onboarding
	Metrics        *obs.ControlMetrics
	ControlStore   store.Store
}

// buildRoot assembles the root mux: console at /ui, control-plane API at /, and
// optional admin, gateway, onboarding, and managed-agent surfaces.
func buildRoot(o rootOptions) http.Handler {
	apiMux := controlplane.NewAPI(o.Registry, o.Metrics, o.ControlStore)
	if o.AdminStore != nil {
		controlplane.RegisterAdmin(apiMux, o.AdminStore, o.Registry.AgentTenants())
		controlplane.RegisterSecretAdmin(apiMux, o.AdminStore, o.SecretAdmin)
		if o.UpstreamStore != nil && o.GatewayMutator != nil {
			controlplane.RegisterUpstreamAdmin(apiMux, o.AdminStore, o.UpstreamStore, o.GatewayMutator)
		}
		if o.AgentStore != nil && o.AgentManager != nil {
			controlplane.RegisterAgentAdmin(apiMux, o.AgentStore, o.AdminStore, o.AgentManager)
		}
	}
	if o.Gateway != nil {
		apiMux.Handle("/gateway/mcp", o.Gateway.HTTP())
		apiMux.HandleFunc("GET /gateway/status", o.Gateway.Status)
	}
	consoleH := console.Handler(o.Registry, o.ControlStore, o.ConsoleOIDC, o.Onboarding)
	root := http.NewServeMux()
	root.Handle("/ui", consoleH)
	root.Handle("/ui/", consoleH)
	root.Handle("/{$}", consoleH)
	root.Handle("/", apiMux)
	return root
}
