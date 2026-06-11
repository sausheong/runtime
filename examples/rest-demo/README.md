# rest-demo — bundled orders API for gateway federation

A tiny, stdlib-only, in-memory REST API that serves its own OpenAPI 3.0.3 spec.
Use it as a live target to prove the gateway's OpenAPI federation end to end.

## Run

```bash
go run ./examples/rest-demo
# listens on :9000 by default; override with RUNTIME_DEMO_ADDR=:9100
```

## Probe it

```bash
curl localhost:9000/orders
curl localhost:9000/orders?status=open
curl localhost:9000/orders/o1
curl -X POST localhost:9000/orders -d '{"item":"cog","qty":3}'
curl localhost:9000/openapi.yaml | head
```

## Federate it through the gateway

Add to `runtime.yaml`:

```yaml
gateway:
  servers:
    - name: orders
      openapi: http://localhost:9000/openapi.yaml
```

The gateway generates one tool per operation, named `<server>__<operationId>`:

| Gateway tool | Operation |
|---|---|
| `orders__listOrders` | `GET /orders` (optional `status` filter) |
| `orders__getOrder` | `GET /orders/{id}` |
| `orders__createOrder` | `POST /orders` |

Agents connected through the gateway MCP endpoint see them as
`mcp__gateway__orders__listOrders`, `mcp__gateway__orders__getOrder`, and
`mcp__gateway__orders__createOrder`.

The `Order` schema lives in `components/schemas` and is referenced via `$ref`
from every operation, which exercises the gateway's schema ref inlining.
