#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <runtime-image> <setup-node-directory> <node-version>" >&2
  exit 64
fi

runtime_image=$1
setup_node_directory=$2
node_version=$3

if [[ ! -f "$setup_node_directory/dist/setup/index.js" ]]; then
  echo "setup-node action is missing dist/setup/index.js: $setup_node_directory" >&2
  exit 66
fi

setup_node_directory=$(cd "$setup_node_directory" && pwd -P)

workflow_commands_token=
if [[ ${GITHUB_ACTIONS:-} == true ]]; then
  workflow_commands_token="runneryard-$(openssl rand -hex 32)"
  echo "::stop-commands::$workflow_commands_token"
fi

set +e
docker run --rm --network none --entrypoint /bin/bash \
  -v "$setup_node_directory:/action:ro" \
  -e "INPUT_NODE-VERSION=$node_version" \
  -e INPUT_ALWAYS-AUTH=false \
  -e "RUNTIME_NODE_VERSION=$node_version" \
  -e RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
  -e RUNNER_TEMP=/tmp/runner \
  -e GITHUB_PATH=/tmp/github-path \
  -e GITHUB_OUTPUT=/tmp/github-output \
  -e GITHUB_ENV=/tmp/github-env \
  -e GITHUB_STATE=/tmp/github-state \
  -e GITHUB_STEP_SUMMARY=/tmp/github-summary \
  "$runtime_image" \
  -lc '
    set -euo pipefail
    case "$(uname -m)" in
      x86_64) toolcache_arch=x64 ;;
      aarch64|arm64) toolcache_arch=arm64 ;;
      *) echo "unsupported canary architecture: $(uname -m)" >&2; exit 1 ;;
    esac
    mkdir -p "$RUNNER_TEMP"
    touch "$GITHUB_PATH" "$GITHUB_OUTPUT" "$GITHUB_ENV" "$GITHUB_STATE" "$GITHUB_STEP_SUMMARY"
    node /action/dist/setup/index.js
    expected_path="$RUNNER_TOOL_CACHE/node/$RUNTIME_NODE_VERSION/$toolcache_arch/bin"
    grep -Fx "$expected_path" "$GITHUB_PATH"
    test "$("$expected_path/node" --version)" = "v$RUNTIME_NODE_VERSION"
  '
canary_status=$?
set -e

if [[ -n $workflow_commands_token ]]; then
  echo "::$workflow_commands_token::"
fi

exit "$canary_status"
