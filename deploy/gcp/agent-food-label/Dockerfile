# Food Label Advisor agent via the Python contract shim.
# Build from the repository root:
#   docker build --platform linux/amd64 \
#     -f deploy/gcp/agent-food-label/Dockerfile \
#     -t food-label-advisor:git-$(git rev-parse --short=12 HEAD) .
#
# The relative path ../../contrib/shims/python from the example dir resolves to
# /app/contrib/shims/python in this layout.
FROM python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91 AS build
COPY --from=ghcr.io/astral-sh/uv:0.11.32@sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c /uv /uvx /bin/
WORKDIR /app
COPY contrib/shims/python /app/contrib/shims/python
COPY examples/food-label-advisor /app/examples/food-label-advisor
WORKDIR /app/examples/food-label-advisor
RUN uv sync --no-dev

FROM python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91
RUN useradd --uid 1000 --create-home agent
WORKDIR /app
COPY --from=build --chown=agent:agent /app /app
WORKDIR /app/examples/food-label-advisor
USER agent
EXPOSE 8310
CMD [".venv/bin/python", "serve.py"]
