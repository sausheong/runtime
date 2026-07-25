# Food Label Advisor agent via the Python contract shim.
# Build from the repository root:
#   docker build --platform linux/amd64 \
#     -f deploy/gcp/agent-food-label/Dockerfile \
#     -t food-label-advisor:git-$(git rev-parse --short=12 HEAD) .
#
# The relative path ../../contrib/shims/python from the example dir resolves to
# /app/contrib/shims/python in this layout.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.16 /uv /uvx /bin/
WORKDIR /app
COPY contrib/shims/python /app/contrib/shims/python
COPY examples/food-label-advisor /app/examples/food-label-advisor
WORKDIR /app/examples/food-label-advisor
RUN uv sync --no-dev
EXPOSE 8310
CMD ["uv", "run", "python", "serve.py"]
