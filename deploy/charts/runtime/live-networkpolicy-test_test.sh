#!/usr/bin/env bash
set -euo pipefail

chart_dir="$(cd "$(dirname "$0")" && pwd)"
subject="${chart_dir}/live-networkpolicy-test.sh"
fake="${chart_dir}/live-networkpolicy-fake-kubectl.sh"

run_case() {
  local scenario="$1"
  RUNTIME_NETWORK_TEST_SCENARIO="$scenario" KUBECTL="$fake" \
    bash "$subject" test-ns source target
}

run_case denied >/dev/null
for scenario in leak dns-failure connection-refused image-pull missing-resource unready; do
  if run_case "$scenario" >/dev/null 2>&1; then
    echo "FAIL: live NetworkPolicy harness accepted ${scenario}" >&2
    exit 1
  fi
done

echo "ok: live NetworkPolicy harness distinguishes policy denial from setup and connectivity failures"
