# Documentation map

This page records where the material from the former long-form `README.md`
now lives. The README is intentionally a concise entry point; this map makes
the split auditable and prevents detailed operational information from being
lost during future edits.

## Start here

| Need | Document |
|---|---|
| Product model, architecture, capabilities, and limitations | [Runtime overview](runtime.md) |
| Complete local installation | [Quickstart](quickstart.md) |
| Production operation and deployment | [Operator guide](operator-guide.md) |
| Tenant onboarding and common workflows | [Tenant guide](tenant-guide.md) |
| Agent configuration, pools, autoscaling, limits, and environment | [Configuration reference](configuration.md) |
| Authentication, roles, keys, secrets, OIDC, and memory | [Identity and memory](identity-and-memory.md) |
| MCP/REST federation and isolated execution tools | [Gateway and sandboxes](gateway-and-sandboxes.md) |
| Metrics, logs, request correlation, traces, and alerts | [Observability](observability.md) |
| Golden sets and online evaluation | [Evaluations](evals.md) |
| CLI commands, HTTP contract, SDK use, durability, and testing | [Interfaces and development](interfaces-and-development.md) |
| Foreign-SDK and remote-agent deployment | [Deploying SDK agents](deploying-sdk-agents.md) |
| TLS-secured host deployment | [Secured deployment](deploy/secured/README.md) |
| Kubernetes deployment | [Helm chart](deploy/charts/runtime/README.md) |
| Known gaps and release blockers | [Roadmap](ROADMAP.md) |

## Former README section recovery

| Former long README section | Current home |
|---|---|
| What Runtime gives you; architecture; concepts; status and scope | [Runtime overview](runtime.md) |
| Quick start; turnkey self-host | [Quickstart](quickstart.md) |
| Configuring agents; replica pools; autoscaling; remote agents; lifecycle limits; pricing | [Configuration reference](configuration.md) |
| Authentication; tenants and roles; OIDC; service keys; open mode | [Identity and memory](identity-and-memory.md) |
| Tenant secrets; key rotation; subject forwarding; OBO | [Identity and memory](identity-and-memory.md) |
| Memory; semantic recall; auto-ingestion; summaries; episodic and actor-scoped memory; garbage collection | [Identity and memory](identity-and-memory.md) |
| MCP gateway; search mode; REST/OpenAPI; credentials; policies; quotas; failure model | [Gateway and sandboxes](gateway-and-sandboxes.md) |
| Code interpreter and browser sandbox; isolation; egress; configuration; limitations | [Gateway and sandboxes](gateway-and-sandboxes.md) |
| Metrics; fan-out; cardinality; request IDs; Prometheus; Grafana; tracing; alerts | [Observability](observability.md) |
| Evaluations; transcript capture; online sampling; failure classification | [Evaluations](evals.md) |
| CLI; web console; HTTP API; agent contract; conformance | [Interfaces and development](interfaces-and-development.md) |
| Native Go SDK; durability; custom agent kinds; foreign SDKs | [Interfaces and development](interfaces-and-development.md) and [Deploying SDK agents](deploying-sdk-agents.md) |
| Nutrition worked example | [Go example](examples/food-label-advisor/README.md), [OpenAI SDK example](examples/nutrition-label-openai/README.md), and [Claude SDK example](examples/nutrition-label-claude/README.md) |
| Deployment; Compose; Helm; operational characteristics | [Operator guide](operator-guide.md), [secured deployment](deploy/secured/README.md), and [Helm chart](deploy/charts/runtime/README.md) |
| Environment variables; YAML blocks; schema | [Configuration reference](configuration.md) |
| Testing; project layout | [Interfaces and development](interfaces-and-development.md) |
| Remaining work | [Roadmap](ROADMAP.md) |

## Documentation maintenance rule

When a capability, endpoint, configuration field, or security boundary changes:

1. update the topic guide that owns it;
2. update the overview if the capability boundary changed;
3. update this map if material moves;
4. run `go test ./internal/doccheck` to validate tracked Markdown links and
   fenced code blocks.

Historical implementation notes may exist in local reports, but the linked
documents above are the maintained public contract.
