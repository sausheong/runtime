# The bundled browser image for Sandboxes M2 (cmd/browserd).
# Build: make browser-image
# Override at runtime with RUNTIME_BROWSER_IMAGE.
FROM debian:bookworm-slim

# Chromium + fonts for headless rendering.
RUN apt-get update && apt-get install -y --no-install-recommends \
        chromium fonts-liberation ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Non-root user; uid must match browserUID in internal/browser/docker.go.
RUN useradd --uid 1000 --create-home browser
USER browser
WORKDIR /home/browser
