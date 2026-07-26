# The bundled sandbox image for Sandboxes M1 (cmd/sandboxd).
# Build: make sandbox-image
# Override at runtime with RUNTIME_SANDBOX_IMAGE.
FROM python:3.13-slim@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91

# Non-root user the containers run as; uid must match sandboxUID in
# internal/sandbox/docker.go.
RUN useradd --uid 1000 --create-home sandbox

# Common analysis libs. `requests` is included deliberately: the library
# exists but the container has no network, so failures demonstrate the
# isolation rather than a missing dependency.
COPY sandbox-requirements.txt /tmp/sandbox-requirements.txt
RUN pip install --no-cache-dir -r /tmp/sandbox-requirements.txt \
    && rm /tmp/sandbox-requirements.txt

USER sandbox
WORKDIR /workspace
