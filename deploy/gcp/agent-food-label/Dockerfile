# Food Label Advisor agent via the Python contract shim.
# Build from the PROJECTS ROOT (parent of runtime/):
#   docker build --platform linux/amd64 \
#     -f runtime/deploy/gcp/agent-food-label/Dockerfile \
#     -t food-label-advisor:latest .
#
# The relative path ../../contrib/shims/python from the example dir resolves to
# /app/contrib/shims/python in this layout.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/
WORKDIR /app
COPY runtime/contrib/shims/python /app/contrib/shims/python
COPY runtime/examples/food-label-advisor /app/examples/food-label-advisor
WORKDIR /app/examples/food-label-advisor
RUN uv sync --no-dev
EXPOSE 8310
CMD ["uv", "run", "python", "serve.py"]
