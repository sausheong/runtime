# A Claude Agent SDK agent (hello-claude), served via the Python contract shim.
# Self-contained: SQLite (shim.db) for durable sessions, no external DB.
#
# Build from the repository root, and for amd64 since the GCP VMs are x86-64
# (the build host may be arm64):
#   docker build --platform linux/amd64 \
#     -f deploy/gcp/agent-claude/Dockerfile \
#     -t hello-claude:git-$(git rev-parse --short=12 HEAD) .
#
# The Claude Agent SDK ships its CLI inside the platform wheel
# (claude_agent_sdk-*-manylinux_2_17_x86_64.whl), so `uv sync` on this amd64 base
# pulls the correct Linux binary automatically — no node, no manual install.
#
# The example's pyproject pins runtime-contract = { path = "../../contrib/shims/python" }.
# The COPY layout below reproduces that relative position: from
# /app/examples/hello-claude, "../../contrib/shims/python" resolves to
# /app/contrib/shims/python.
FROM python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91 AS build
COPY --from=ghcr.io/astral-sh/uv:0.11.32@sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c /uv /uvx /bin/
WORKDIR /app
COPY contrib/shims/python /app/contrib/shims/python
COPY examples/hello-claude /app/examples/hello-claude
WORKDIR /app/examples/hello-claude
RUN uv sync --no-dev

FROM python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91
RUN useradd --uid 1000 --create-home agent
WORKDIR /app
COPY --from=build --chown=agent:agent /app /app
WORKDIR /app/examples/hello-claude
USER agent
EXPOSE 8080
# serve.py reads RUNTIME_LISTEN_ADDR / RUNTIME_AGENT_ID / RUNTIME_SHIM_DB and the
# ANTHROPIC_* vars from the environment (injected by docker-compose.yml).
CMD [".venv/bin/python", "serve.py"]
