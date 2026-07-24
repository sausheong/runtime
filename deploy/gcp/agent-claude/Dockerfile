# A Claude Agent SDK agent (hello-claude), served via the Python contract shim.
# Self-contained: SQLite (shim.db) for durable sessions, no external DB.
#
# Build from the PROJECTS ROOT (parent of runtime/), and for amd64 since the GCP
# VMs are x86-64 (build host may be arm64):
#   docker build --platform linux/amd64 \
#     -f runtime/deploy/gcp/agent-claude/Dockerfile \
#     -t hello-claude:latest .
#
# The Claude Agent SDK ships its CLI inside the platform wheel
# (claude_agent_sdk-*-manylinux_2_17_x86_64.whl), so `uv sync` on this amd64 base
# pulls the correct Linux binary automatically — no node, no manual install.
#
# The example's pyproject pins runtime-contract = { path = "../../contrib/shims/python" }.
# The COPY layout below reproduces that relative position: from
# /app/examples/hello-claude, "../../contrib/shims/python" resolves to
# /app/contrib/shims/python.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.16 /uv /uvx /bin/
WORKDIR /app
COPY runtime/contrib/shims/python /app/contrib/shims/python
COPY runtime/examples/hello-claude /app/examples/hello-claude
WORKDIR /app/examples/hello-claude
RUN uv sync --no-dev
EXPOSE 8080
# serve.py reads RUNTIME_LISTEN_ADDR / RUNTIME_AGENT_ID / RUNTIME_SHIM_DB and the
# ANTHROPIC_* vars from the environment (injected by docker-compose.yml).
CMD ["uv", "run", "python", "serve.py"]
