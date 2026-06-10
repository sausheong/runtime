# The bundled sandbox image for Sandboxes M1 (cmd/sandboxd).
# Build: make sandbox-image
# Override at runtime with RUNTIME_SANDBOX_IMAGE.
FROM python:3.12-slim

# Non-root user the containers run as; uid must match sandboxUID in
# internal/sandbox/docker.go.
RUN useradd --uid 1000 --create-home sandbox

# Common analysis libs. `requests` is included deliberately: the library
# exists but the container has no network, so failures demonstrate the
# isolation rather than a missing dependency.
RUN pip install --no-cache-dir numpy pandas matplotlib requests

USER sandbox
WORKDIR /workspace
