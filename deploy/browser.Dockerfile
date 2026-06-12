# The bundled browser image for Sandboxes M2 (cmd/browserd).
# Build: make browser-image
# Override at runtime with RUNTIME_BROWSER_IMAGE.
FROM debian:bookworm-slim

# Chromium + fonts for headless rendering; socat bridges the published CDP
# port to Chromium's loopback-only DevTools socket (Chromium ignores
# --remote-debugging-address and always binds 127.0.0.1).
RUN apt-get update && apt-get install -y --no-install-recommends \
        chromium fonts-liberation ca-certificates socat \
    && rm -rf /var/lib/apt/lists/*

# Non-root user; uid must match browserUID in internal/browser/docker.go.
RUN useradd --uid 1000 --create-home browser

# Entrypoint: socat relays 0.0.0.0:9222 (published) -> 127.0.0.1:9221 (Chromium
# DevTools). Chromium runs in the foreground so the container's lifecycle
# tracks the browser. RUNTIME_CHROME_PROXY is the egress proxy URL.
RUN printf '%s\n' \
  '#!/bin/sh' \
  'set -e' \
  'socat tcp-listen:9222,fork,reuseaddr,bind=0.0.0.0 tcp:127.0.0.1:9221 &' \
  'exec chromium --headless=new --no-sandbox --disable-gpu \' \
  '  --remote-debugging-address=127.0.0.1 --remote-debugging-port=9221 \' \
  '  --proxy-server="$RUNTIME_CHROME_PROXY" \' \
  '  --disable-blink-features=AutomationControlled \' \
  '  --user-data-dir=/profile --no-first-run --no-default-browser-check \' \
  '  about:blank' \
  > /usr/local/bin/browser-entrypoint.sh \
  && chmod +x /usr/local/bin/browser-entrypoint.sh

USER browser
WORKDIR /home/browser
ENTRYPOINT ["/usr/local/bin/browser-entrypoint.sh"]
