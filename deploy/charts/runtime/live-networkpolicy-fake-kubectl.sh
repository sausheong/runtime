#!/usr/bin/env bash
set -euo pipefail

scenario="${RUNTIME_NETWORK_TEST_SCENARIO:-denied}"
args=" $* "

if [[ "$1" == "get" && "$2" == "pods" ]]; then
  if [[ "$args" == *"app.kubernetes.io/instance=source"* ]]; then
    echo "source-agent-0"
  elif [[ "$args" == *"app.kubernetes.io/component=control-plane"* ]]; then
    echo "target-control-0"
  else
    echo "target-agent-0"
  fi
  exit 0
fi

if [[ "$1" == "get" && "$2" == "pod" ]]; then
  pod="$5"
  if [[ "$args" == *".status.conditions"* ]]; then
    if [[ "$scenario" == "unready" && "$pod" == "source-agent-0" ]]; then
      echo "False"
    else
      echo "True"
    fi
  elif [[ "$pod" == "source-agent-0" ]]; then
    echo "source-agent"
  elif [[ "$pod" == "target-agent-0" ]]; then
    echo "target-agent"
  fi
  exit 0
fi

if [[ "$1" == "get" && "$2" == "service" ]]; then
  if [[ "$scenario" == "missing-resource" && "$args" == *"runtime.sausheong.io/metrics=true"* ]]; then
    exit 0
  fi
  if [[ "$args" == *"runtime.sausheong.io/metrics=true"* ]]; then
    echo "target-metrics"
  elif [[ "$args" == *"runtime.agent/id=target-agent"* ]]; then
    echo "target-agent"
  elif [[ "$args" == *"metadata.labels.runtime"* ]]; then
    printf 'source-control||8080\nsource-agent|source-agent|8080\n'
  else
    echo "source-control"
  fi
  exit 0
fi

if [[ "$1" == "debug" ]]; then
  if [[ "$scenario" == "image-pull" && "$args" == *"pod/source-agent-0"* ]]; then
    echo "unable to pull debug image" >&2
    exit 1
  fi
  url="${!#}"
  rc=0
  if [[ "$url" == *"target-metrics"* ]]; then
    if [[ "$args" == *"pod/source-agent-0"* ]]; then
      rc=28
    else
      echo 'runtime_agent_up{agent="target-agent",replica="0"} 1'
    fi
  elif [[ "$url" == *"target-agent"* && "$args" == *"pod/source-agent-0"* ]]; then
    rc=28
  fi
  if [[ "$scenario" == "dns-failure" && "$args" == *"pod/source-agent-0"* ]]; then
    rc=6
  elif [[ "$scenario" == "connection-refused" && "$args" == *"pod/source-agent-0"* &&
          "$url" != *"source-control"* ]]; then
    rc=7
  elif [[ "$scenario" == "leak" && "$args" == *"pod/source-agent-0"* &&
          "$url" != *"source-control"* ]]; then
    rc=0
  fi
  echo "__RUNTIME_CURL_EXIT__=${rc}"
  exit 0
fi

echo "unexpected fake kubectl invocation: $*" >&2
exit 1
