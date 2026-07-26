# Instance C — OpenAI Agents SDK nutrition agent, served via the Python contract
# shim. Self-contained: SQLite (shim.db) for Level-1 durability, no external DB.
#
# Build from the repository root, and for amd64 since the GCP VMs are x86-64
# (the build host may be arm64):
#   docker build --platform linux/amd64 \
#     -f deploy/gcp/agent-python/Dockerfile \
#     -t nutrition-openai:git-$(git rev-parse --short=12 HEAD) .
#
# The example's pyproject pins runtime-contract = { path = "../../contrib/shims/python" }.
# The COPY layout below reproduces that relative position: from
# /app/examples/nutrition-label-openai, "../../contrib/shims/python" resolves to
# /app/contrib/shims/python.
FROM python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91 AS build
COPY --from=ghcr.io/astral-sh/uv:0.11.32@sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c /uv /uvx /bin/
WORKDIR /app
COPY contrib/shims/python /app/contrib/shims/python
COPY examples/nutrition-label-openai /app/examples/nutrition-label-openai
WORKDIR /app/examples/nutrition-label-openai
RUN uv sync --no-dev

FROM python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91
RUN useradd --uid 1000 --create-home agent
WORKDIR /app
COPY --from=build --chown=agent:agent /app /app
WORKDIR /app/examples/nutrition-label-openai
USER agent
EXPOSE 8302
# serve.py reads RUNTIME_LISTEN_ADDR / RUNTIME_AGENT_ID / RUNTIME_SHIM_DB and the
# OPENAI_* vars from the environment (injected by docker-compose.yml).
CMD [".venv/bin/python", "serve.py"]
