#!/usr/bin/env bash
set -euo pipefail

# Opt-in acceptance test for a deployed perAgentPods release. It adds temporary
# curl debug containers to existing pods so the requests originate with the
# pods' real labels/IPs and are evaluated by NetworkPolicy.
#
# Usage: live-networkpolicy-test.sh <namespace> <release>
namespace="${1:?namespace is required}"
release="${2:?Helm release name is required}"
selector="app.kubernetes.io/instance=${release}"

mapfile -t agent_pods < <(
  kubectl get pods -n "$namespace" -l "$selector" \
    -o jsonpath='{range .items[?(@.metadata.labels.runtime\.agent/id)]}{.metadata.name}{"\n"}{end}'
)
if (( ${#agent_pods[@]} < 2 )); then
  echo "need at least two ready agent pods for a cross-agent test" >&2
  exit 1
fi

source_pod="${agent_pods[0]}"
target_pod="${agent_pods[1]}"
target_id="$(
  kubectl get pod -n "$namespace" "$target_pod" \
    -o jsonpath='{.metadata.labels.runtime\.agent/id}'
)"
target_service="$(
  kubectl get service -n "$namespace" \
    -l "${selector},runtime.agent/id=${target_id}" \
    -o jsonpath='{.items[0].metadata.name}'
)"
control_pod="$(
  kubectl get pods -n "$namespace" \
    -l "${selector},app.kubernetes.io/component=control-plane" \
    -o jsonpath='{.items[0].metadata.name}'
)"
metrics_service="$(
  kubectl get service -n "$namespace" \
    -l "${selector},runtime.sausheong.io/metrics=true" \
    -o jsonpath='{.items[0].metadata.name}'
)"
test_url="http://${target_service}:8080/healthz"

if kubectl debug -n "$namespace" "pod/${source_pod}" \
  --quiet --image=curlimages/curl:8.12.1 --container="deny-probe-$RANDOM" -- \
  curl --fail --silent --show-error --connect-timeout 3 --max-time 5 "$test_url"
then
  echo "cross-agent request unexpectedly reached ${target_id}" >&2
  exit 1
fi

kubectl debug -n "$namespace" "pod/${control_pod}" \
  --quiet --image=curlimages/curl:8.12.1 --container="allow-probe-$RANDOM" -- \
  curl --fail --silent --show-error --connect-timeout 3 --max-time 5 "$test_url" >/dev/null

metrics="$(
  kubectl debug -n "$namespace" "pod/${control_pod}" \
    --quiet --image=curlimages/curl:8.12.1 --container="metrics-probe-$RANDOM" -- \
    curl --fail --silent --show-error --connect-timeout 3 --max-time 10 \
      "http://${metrics_service}:9091/metrics"
)"
if ! grep -Eq "runtime_agent_up\\{agent=\"${target_id}\",replica=\"[0-9]+\"\\} 1" <<<"$metrics"; then
  echo "management metrics did not contain a successful signed scrape for ${target_id}" >&2
  exit 1
fi

echo "OK: ${source_pod} was denied, ${control_pod} reached ${target_service}, and signed agent metrics were collected"
