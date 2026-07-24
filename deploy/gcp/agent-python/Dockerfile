# Instance C — OpenAI Agents SDK nutrition agent, served via the Python contract
# shim. Self-contained: SQLite (shim.db) for Level-1 durability, no external DB.
#
# Build from the PROJECTS ROOT (parent of runtime/), and for amd64 since the GCP
# VMs are x86-64 (build host may be arm64):
#   docker build --platform linux/amd64 \
#     -f runtime/deploy/gcp/agent-python/Dockerfile \
#     -t nutrition-openai:latest .
#
# The example's pyproject pins runtime-contract = { path = "../../contrib/shims/python" }.
# The COPY layout below reproduces that relative position: from
# /app/examples/nutrition-label-openai, "../../contrib/shims/python" resolves to
# /app/contrib/shims/python.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.16 /uv /uvx /bin/
WORKDIR /app
COPY runtime/contrib/shims/python /app/contrib/shims/python
COPY runtime/examples/nutrition-label-openai /app/examples/nutrition-label-openai
WORKDIR /app/examples/nutrition-label-openai
RUN uv sync --no-dev
EXPOSE 8302
# serve.py reads RUNTIME_LISTEN_ADDR / RUNTIME_AGENT_ID / RUNTIME_SHIM_DB and the
# OPENAI_* vars from the environment (injected by docker-compose.yml).
CMD ["uv", "run", "python", "serve.py"]
