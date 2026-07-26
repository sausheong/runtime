#!/usr/bin/env bash
set -euo pipefail

# Opt-in acceptance test for two deployed one-agent perAgentPods releases.
# Ephemeral curl containers run inside the selected pods, so traffic carries the
# real pod labels/IPs evaluated by NetworkPolicy.
#
# Usage: live-networkpolicy-test.sh <namespace> <source-release> <target-release>
namespace="${1:?namespace is required}"
source_release="${2:?source Helm release name is required}"
target_release="${3:?target Helm release name is required}"
kubectl_bin="${KUBECTL:-kubectl}"
debug_image="${RUNTIME_NETWORK_TEST_IMAGE:-curlimages/curl:8.12.1}"
source_selector="app.kubernetes.io/instance=${source_release}"
target_selector="app.kubernetes.io/instance=${target_release}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

first_ready_pod() {
  local selector="$1"
  local require_agent="${2:-false}"
  local pods pod ready agent_id
  pods="$("$kubectl_bin" get pods -n "$namespace" -l "$selector" \
    --field-selector=status.phase=Running \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"
  for pod in $pods; do
    ready="$("$kubectl_bin" get pod -n "$namespace" "$pod" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    [[ "$ready" == "True" ]] || continue
    if [[ "$require_agent" == "true" ]]; then
      agent_id="$("$kubectl_bin" get pod -n "$namespace" "$pod" \
        -o jsonpath='{.metadata.labels.runtime\.agent/id}')"
      [[ -n "$agent_id" ]] || continue
    fi
    printf '%s\n' "$pod"
    return 0
  done
  return 1
}

first_service() {
  local selector="$1"
  local name
  name="$("$kubectl_bin" get service -n "$namespace" -l "$selector" \
    -o jsonpath='{.items[0].metadata.name}')"
  [[ -n "$name" ]] || return 1
  printf '%s\n' "$name"
}

control_service_info() {
  local selector="$1"
  local rows name agent_id port found_name="" found_port="" count=0
  rows="$("$kubectl_bin" get service -n "$namespace" -l "$selector" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.labels.runtime\.agent/id}{"|"}{.spec.ports[?(@.name=="http")].port}{"\n"}{end}')"
  while IFS='|' read -r name agent_id port; do
    [[ -n "$name" && -z "$agent_id" && -n "$port" ]] || continue
    found_name="$name"
    found_port="$port"
    count=$((count + 1))
  done <<<"$rows"
  [[ "$count" -eq 1 ]] || return 1
  printf '%s %s\n' "$found_name" "$found_port"
}

# run_probe separates kubectl/ephemeral-container setup from curl's result.
# The debug command itself exits zero and emits a marker for every curl outcome;
# absence of that marker is an infrastructure/setup failure, never a denial.
PROBE_OUTPUT=""
PROBE_RC=""
run_probe() {
  local pod="$1"
  local name="$2"
  local url="$3"
  local output marker
  if ! output="$("$kubectl_bin" debug -n "$namespace" "pod/${pod}" \
    --quiet --image="$debug_image" --container="${name}-${RANDOM}" -- \
    sh -c 'curl --fail --silent --show-error --connect-timeout 3 --max-time 10 "$1"; rc=$?; printf "\n__RUNTIME_CURL_EXIT__=%s\n" "$rc"; exit 0' \
    runtime-network-probe "$url" 2>&1)"; then
    fail "debug probe setup failed for pod ${pod}: ${output}"
  fi
  marker="$(sed -n 's/^__RUNTIME_CURL_EXIT__=\([0-9][0-9]*\)$/\1/p' <<<"$output" | tail -1)"
  [[ -n "$marker" ]] ||
    fail "debug probe on pod ${pod} produced no curl exit marker: ${output}"
  PROBE_RC="$marker"
  PROBE_OUTPUT="$(sed '/^__RUNTIME_CURL_EXIT__=[0-9][0-9]*$/d' <<<"$output")"
}

expect_allowed() {
  local description="$1"
  [[ "$PROBE_RC" == "0" ]] ||
    fail "${description} failed with curl exit ${PROBE_RC}: ${PROBE_OUTPUT}"
}

expect_policy_denied() {
  local description="$1"
  # This acceptance requires the timeout produced by dropped traffic. Connection
  # refused (7) can mean an unready endpoint and is deliberately not accepted;
  # DNS failure (6), HTTP failure (22), and every other result also fail.
  case "$PROBE_RC" in
    28) ;;
    0) fail "${description} unexpectedly succeeded" ;;
    *) fail "${description} failed for a non-policy reason (curl exit ${PROBE_RC}): ${PROBE_OUTPUT}" ;;
  esac
}

source_pod="$(first_ready_pod "$source_selector" true)" ||
  fail "source release has no Running, Ready agent pod"
target_pod="$(first_ready_pod "$target_selector" true)" ||
  fail "target release has no Running, Ready agent pod"
target_control_pod="$(first_ready_pod "${target_selector},app.kubernetes.io/component=control-plane")" ||
  fail "target release has no Running, Ready control-plane pod"

source_id="$("$kubectl_bin" get pod -n "$namespace" "$source_pod" \
  -o jsonpath='{.metadata.labels.runtime\.agent/id}')"
target_id="$("$kubectl_bin" get pod -n "$namespace" "$target_pod" \
  -o jsonpath='{.metadata.labels.runtime\.agent/id}')"
[[ -n "$source_id" && -n "$target_id" && "$source_id" != "$target_id" ]] ||
  fail "source and target releases must carry distinct non-empty agent IDs"

read -r source_control_service source_control_port < <(control_service_info "$source_selector") ||
  fail "source control-plane Service was not found"
target_agent_service="$(first_service "${target_selector},runtime.agent/id=${target_id}")" ||
  fail "target agent Service was not found"
target_metrics_service="$(first_service "${target_selector},runtime.sausheong.io/metrics=true")" ||
  fail "target management-metrics Service was not found"

source_baseline_url="http://${source_control_service}:${source_control_port}/healthz"
target_agent_url="http://${target_agent_service}:8080/healthz"
target_metrics_url="http://${target_metrics_service}:9091/metrics"

# Establish both sides before interpreting a denial. These checks prove the
# image can start, the source can resolve cluster DNS and make an ordinary
# request, and the target endpoints are live from an allowed peer.
run_probe "$source_pod" source-baseline "$source_baseline_url"
expect_allowed "source debug/DNS/connectivity baseline"
run_probe "$target_control_pod" target-agent-allowed "$target_agent_url"
expect_allowed "target control-plane to agent"
run_probe "$target_control_pod" target-metrics-allowed "$target_metrics_url"
expect_allowed "target control-plane to management metrics"
grep -Eq "runtime_agent_up\\{agent=\"${target_id}\",replica=\"[0-9]+\"\\} 1" <<<"$PROBE_OUTPUT" ||
  fail "management metrics lacked a successful signed scrape for ${target_id}"

run_probe "$source_pod" cross-agent-deny "$target_agent_url"
expect_policy_denied "agent ${source_id} to agent ${target_id}"
run_probe "$source_pod" metrics-deny "$target_metrics_url"
expect_policy_denied "agent ${source_id} to ${target_release} management metrics"

echo "OK: validated source connectivity, target allowed paths, and policy denial from agent ${source_id} to agent ${target_id} and management metrics"
