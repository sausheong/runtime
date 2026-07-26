# Identity, secrets, and memory

Runtime enforces authentication and tenant ownership at the control-plane edge.
Agents therefore receive only traffic already attributed to a principal, but
agent and tool implementations must still preserve tenant context when they
read external systems.

## Security model

A principal has a subject, tenant, and role:

| Role | Typical access |
|---|---|
| `viewer` | Inspect agents, sessions, and permitted operator views |
| `operator` | Create sessions, send messages, and perform normal agent operations |
| `admin` | Manage tenant users, keys, secrets, agents, upstreams, policies, quotas, and evaluations |

A platform superuser can administer multiple tenants. Tenant administrators
remain constrained to their own tenant. The API deliberately distinguishes:

- `401` for absent or invalid credentials;
- `403` for a known principal without the required role;
- `404` when a resource belongs to another tenant, to avoid disclosing it.

Session routes check both the requested agent tenant and the persisted session
owner. Dynamic agent mutations resolve the existing object before authorising
the change.

## Human login with OIDC

Set at least:

```bash
RUNTIME_OIDC_ISSUER=https://identity.example.com
RUNTIME_OIDC_CLIENT_ID=runtime
RUNTIME_OIDC_CLIENT_SECRET=...
RUNTIME_OIDC_REDIRECT_URL=https://runtime.example.com/auth/callback
```

The console uses the OIDC authorisation-code flow. Users must also have a
Runtime user mapping that assigns their issuer subject to a tenant and role.
OIDC authenticates the person; it does not replace Runtime's tenant
authorisation data.

Set `RUNTIME_PUBLIC_URL` to the externally visible HTTPS origin when Runtime is
behind a proxy. Runtime infers secure cookies from HTTPS public/OIDC URLs;
`RUNTIME_COOKIE_SECURE=1` forces them on. The deployment edge must terminate
TLS and forward requests without exposing the private management listener.

## Service keys and bootstrap

Applications and automation use platform-issued bearer keys:

```bash
export RUNTIME_CTL_URL=https://runtime.example.com
export RUNTIME_TOKEN=<admin-or-operator-key>

runtimectl admin tenant create acme --name "Acme"
runtimectl admin user add oidc-subject --role admin --tenant acme
runtimectl admin key create --role operator --label app --tenant acme
```

The plaintext service key is returned once. Runtime stores only a verifier.
List and revoke keys with:

```bash
runtimectl admin key ls
runtimectl admin key revoke <key-id>
```

`RUNTIME_ADMIN_BOOTSTRAP` exists to create the first durable administrator. Once
durable identity credentials exist, the bootstrap credential is ignored.
`RUNTIME_ADMIN_BREAK_GLASS=1` temporarily re-enables it and should be set only
for a controlled recovery window, then removed and followed by a restart.

Legacy static `tokens:` entries in `runtime.yaml` remain compatibility
credentials. Prefer durable service keys for production because they can be
attributed and revoked without editing the file.

## Open mode

When neither OIDC nor durable/static credentials are configured, Runtime can
operate without authentication for local development. In open mode there is no
meaningful hostile-tenant boundary. Never expose it to an untrusted network,
and do not treat it as a production configuration.

## Tenant secrets

The secret broker stores named values per tenant. It encrypts values with
AES-256-GCM, returns metadata rather than plaintext through the API, and
resolves secrets only at the point where a managed agent or gateway upstream
needs them.

Configure a keyring:

```bash
RUNTIME_SECRETS_KEYS='primary=<base64-32-byte-key>,old=<base64-32-byte-key>'
RUNTIME_SECRETS_PRIMARY=primary
```

The legacy `RUNTIME_SECRETS_KEY` accepts one key but does not provide clean
online rotation. Protect keys independently of the database: a database backup
without the keyring cannot decrypt stored credentials, while a database and
keyring together contain the secrets.

Set ordinary values without exposing them in shell history:

```bash
printf '%s' "$OPENAI_API_KEY" |
  runtimectl admin secret set OPENAI_API_KEY --value-stdin --tenant acme

runtimectl admin secret ls
runtimectl admin secret rm OPENAI_API_KEY
```

Managed child processes receive a deliberately small platform environment,
then their tenant's brokered secrets. Use `RUNTIME_AGENT_PG_DSN` for a
restricted database role; identity-enabled Runtime fails closed when it is the
same DSN as the control-plane credential. Runtime maps that restricted login
to the configured local-agent tenant and PostgreSQL row-level policies prevent
it from reading another tenant's sessions, events, transcripts, or online
results. One restricted login must not be shared by concurrently running local
agents from different tenants; run a separate Runtime deployment, database,
and role for each local-agent tenant. Additional operator variables must be explicitly named in
`RUNTIME_AGENT_ENV_PASSTHROUGH`; reserved Runtime variables cannot be
overridden through that mechanism.

### Key rotation

Safe rotation is:

1. Add a new key ID to `RUNTIME_SECRETS_KEYS`.
2. Set `RUNTIME_SECRETS_PRIMARY` to the new ID.
3. Restart Runtime so new writes use it.
4. Re-encrypt stored records:

   ```bash
   runtimectl admin secret rotate
   ```

5. Verify all tenant records and managed workloads.
6. Remove the old key only after no record references it and backups follow the
   organisation's retention policy.

Rotation can be tenant-scoped with `--tenant` when acting as a platform
superuser.

## OAuth2 and on-behalf-of credentials

Gateway upstreams can use a static secret or a structured OAuth credential.
Client-credentials configuration:

```bash
printf '%s' "$CLIENT_SECRET" |
  runtimectl admin secret set-oauth2 \
    --name orders-oauth \
    --token-url https://id.example.com/oauth/token \
    --client-id runtime \
    --client-secret-stdin \
    --scope orders.read
```

For OAuth token exchange/on-behalf-of:

```bash
printf '%s' "$CLIENT_SECRET" |
  runtimectl admin secret set-obo \
    --name orders-obo \
    --token-url https://id.example.com/oauth/token \
    --client-id runtime \
    --client-secret-stdin \
    --scope orders.read
```

Runtime mints the outbound token at call time and fails closed if the credential
cannot be resolved. OBO requires an authenticated caller subject token; a
machine key without a suitable subject token cannot impersonate a person.

`RUNTIME_SUBJECT_FORWARDING=1` enables forwarding of authenticated caller
identity to native agents. Runtime signs each projection with an Ed25519 private
key held only by the control plane. Agents receive the matching public key.
Signatures cover the HTTP method, request target, timestamp, nonce, and
forwarded claims; the agent rejects expired signatures and nonce replay. A
per-agent authentication bearer therefore cannot mint caller identities.

For local processes and registration-handshake agents, Runtime can generate an
ephemeral key pair at control-plane boot. Independently started remote agents
and Kubernetes deployments need a stable pair:

```bash
openssl genpkey -algorithm Ed25519 -out runtime-identity-signing.pem
RUNTIME_IDENTITY_SIGNING_PRIVATE_KEY="$(
  openssl pkey -in runtime-identity-signing.pem -outform DER |
    tail -c 32 | openssl base64 -A | tr '+/' '-_' | tr -d '='
)"
RUNTIME_IDENTITY_SIGNING_PUBLIC_KEY="$(
  openssl pkey -in runtime-identity-signing.pem -pubout -outform DER |
    tail -c 32 | openssl base64 -A | tr '+/' '-_' | tr -d '='
)"
export RUNTIME_IDENTITY_SIGNING_PRIVATE_KEY
export RUNTIME_IDENTITY_SIGNING_PUBLIC_KEY
```

Keep the private value out of agent environments. Use subject forwarding only
when the agent contract and downstream trust model require it. An
operator-controlled `forward_tenant` setting may inject `__rt_tenant` into
stdio gateway tool arguments; Runtime strips any caller-supplied value first.

## Durable memory

Set `memory: true` on a local native agent to register the memory tool. Records
are durably stored in Postgres and scoped by tenant. The current platform
boundary is tenant-level; it is not yet a general per-user or per-agent
authorisation boundary. Actor namespacing improves relevance when subject
forwarding is enabled but does not replace access control.

Without embeddings, agents can still save and retrieve explicit memories.
Embedding configuration adds semantic recall:

```bash
RUNTIME_EMBED_MODEL=text-embedding-3-small
RUNTIME_EMBED_DIM=1536
RUNTIME_EMBED_RECALL_K=5
RUNTIME_EMBED_RECALL_FLOOR=0.25
```

`RUNTIME_EMBED_MODEL` and `RUNTIME_EMBED_DIM` must match the provider and the
pgvector column. Runtime uses the OpenAI-compatible `OPENAI_BASE_URL` and
`OPENAI_API_KEY`. Measure similarity distributions for the selected embedding
family before raising the floor: unrelated and useful text can occupy very
different ranges across models.

### Automatic fact ingestion

```bash
RUNTIME_INGEST_ENABLED=1
RUNTIME_INGEST_MODEL=<chat-model>
RUNTIME_INGEST_MIN_MESSAGES=2
RUNTIME_INGEST_MAX_FACTS=8
RUNTIME_INGEST_MAX_INFLIGHT=4
RUNTIME_INGEST_DEDUP_FLOOR=0.85
```

After a completed turn, the extractor can derive durable facts and deduplicate
them by embedding similarity. It is best-effort: extraction failure must not
fail the user turn. Ingestion requires embeddings. Cap in-flight extraction to
protect the model provider and database during bursts. Accepted ingestion is
owned by the agent lifecycle. Shutdown stops new admission, gives cooperative
workers a bounded drain period, cancels overdue extraction and storage calls,
and returns without waiting forever for a dependency that ignores
cancellation. The memory database remains open until that drain completes.

### Rolling summaries

```bash
RUNTIME_SUMMARY_ENABLED=1
RUNTIME_SUMMARY_MODEL=<chat-model>
RUNTIME_SUMMARY_MIN_MESSAGES=2
```

If no summary model is set, Runtime falls back to
`RUNTIME_INGEST_MODEL`. A summary supersedes the previous rolling summary for
that session. Summary-only operation does not require embeddings.

### Episodic memory

```bash
RUNTIME_EPISODIC_ENABLED=1
RUNTIME_EPISODIC_MODEL=<chat-model>
RUNTIME_EPISODIC_MIN_MESSAGES=2
RUNTIME_EPISODIC_MAX=8
RUNTIME_EPISODIC_RECALL_K=3
```

Episodes capture structured experience for later semantic recall. They require
embeddings. The episodic model falls back to the ingestion model.

### Garbage collection

Dead superseded and tombstoned memory rows are reaped by default:

| Variable | Default |
|---|---|
| `RUNTIME_MEMORY_GC_ENABLED` | Enabled when memory is enabled |
| `RUNTIME_MEMORY_GC_INTERVAL` | `1h` |
| `RUNTIME_MEMORY_GC_GRACE` | `24h` |
| `RUNTIME_MEMORY_GC_BATCH` | `1000` |

The grace period protects concurrent readers and gives operators time to
observe accidental supersession. Disabling GC permits unbounded dead-row
growth.

Live memory is retained indefinitely unless an operator opts into per-kind
retention:

| Variable | Default |
|---|---|
| `RUNTIME_MEMORY_RETENTION_FACT` | Disabled |
| `RUNTIME_MEMORY_RETENTION_SUMMARY` | Disabled |
| `RUNTIME_MEMORY_RETENTION_EPISODE` | Disabled |
| `RUNTIME_MEMORY_RETENTION_DRY_RUN` | Disabled |

Durations use Go duration syntax such as `720h`. Dry-run mode reports eligible
rows without deleting them or incrementing deletion counters. Locally managed
agents inherit these safe maintenance controls from `runtimed`; remote agents
must set them in their own deployment environment.

## Memory observability and limitations

Useful metrics include:

- `agent_memory_summary_writes_total`;
- `agent_memory_episode_writes_total`;
- `agent_memory_gc_deleted_total`;
- `agent_memory_retention_reaped_total{kind=...}`.

Extraction, summarisation, and episodic generation send conversation-derived
content to the configured model provider. Operators must apply their data
classification, retention, and residency requirements. Memory is not a
substitute for a domain database, record-level ACLs, or an audit system.
