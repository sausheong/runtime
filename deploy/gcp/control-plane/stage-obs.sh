#!/usr/bin/env bash
# Populate ./obs/ with the observability config the control-plane compose mounts
# (prometheus, otel collector, grafana provisioning + dashboards). Run this from
# deploy/gcp/control-plane/ BEFORE scp'ing this directory to instance A, so the
# bundle is self-contained (the compose mounts ./obs/..., never ../../). Re-runs
# overwrite. Resolves paths relative to the repo, so it works from any checkout.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"   # deploy/gcp/control-plane -> repo root

rm -rf "$here/obs"
mkdir -p "$here/obs/grafana"
cp "$repo/deploy/compose/prometheus.yml"       "$here/obs/prometheus.yml"
cp "$repo/deploy/compose/alertmanager.yml"     "$here/obs/alertmanager.yml"
cp -R "$repo/deploy/compose/rules"             "$here/obs/rules"
cp "$repo/deploy/jaeger-config.yaml"           "$here/obs/jaeger-config.yaml"
cp "$repo/deploy/otel/collector-config.yaml"   "$here/obs/collector-config.yaml"
cp -R "$repo/deploy/grafana/provisioning"      "$here/obs/grafana/provisioning"
cp -R "$repo/deploy/grafana/dashboards"        "$here/obs/grafana/dashboards"

echo "staged ./obs from $repo/deploy/{compose,otel,grafana}:"
find "$here/obs" -type f | sed "s|$here/|  |"
