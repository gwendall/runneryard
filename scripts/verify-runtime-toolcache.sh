#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 || $# -gt 4 ]]; then
  echo "usage: $0 <runtime-image> <setup-node-directory> <node-version> [platform]" >&2
  exit 64
fi

runtime_image=$1
setup_node_directory=$2
node_version=$3
platform=${4:-}

if [[ ! $node_version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "node version must be an exact numeric patch: $node_version" >&2
  exit 65
fi

docker_platform_args=()
if [[ -n $platform ]]; then
  docker_platform_args=(--platform "$platform")
fi

if [[ ! -f "$setup_node_directory/dist/setup/index.js" ]]; then
  echo "setup-node action is missing dist/setup/index.js: $setup_node_directory" >&2
  exit 66
fi

setup_node_directory=$(cd "$setup_node_directory" && pwd -P)

# Buildx is a client-side plugin, so its version can be verified without a
# daemon or network. The worker entrypoint enables BuildKit for job steps.
docker run --rm --network none "${docker_platform_args[@]}" --entrypoint docker \
  "$runtime_image" buildx version
docker run --rm --network none "${docker_platform_args[@]}" --entrypoint docker \
  "$runtime_image" compose version

# Exercise the real runner privilege transition. The fake daemon isolates this
# canary from the host while the mounted run.sh observes the exact environment
# received by a GitHub job. Do not pass the toolcache variables to docker run:
# they must come from runner-entrypoint after sudo/setpriv.
entrypoint_canary_directory=$(mktemp -d)
cleanup_entrypoint_canary() {
  rm -rf "$entrypoint_canary_directory"
}
trap cleanup_entrypoint_canary EXIT
mkdir -p "$entrypoint_canary_directory/output"
chmod 0777 "$entrypoint_canary_directory/output"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'touch /var/run/docker.sock' \
  'cp /etc/docker/daemon.json /canary-output/daemon.json' \
  'trap "exit 0" TERM INT' \
  'while true; do sleep 60 & wait $!; done' \
  >"$entrypoint_canary_directory/dockerd"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'test "${1:-}" = info' \
  'touch /var/run/docker.sock' \
  'if [[ "${2:-}" == --format ]]; then cat /canary-output/storage-driver; fi' \
  >"$entrypoint_canary_directory/docker"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'test "${*: -1}" = /var/lib/docker' \
  "printf '%s\\n' ext4" \
  >"$entrypoint_canary_directory/findmnt"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  "expected_node_version=$node_version" \
  'test "$(id -u)" = 1001' \
  'test "$HOME" = /home/runner' \
  'test "$RUNNER_TOOL_CACHE" = /opt/hostedtoolcache' \
  'test "$ImageOS" = ubuntu24' \
  'test "$DOCKER_BUILDKIT" = 1' \
  'case "$(uname -m)" in' \
  '  x86_64) toolcache_arch=x64 ;;' \
  '  aarch64|arm64) toolcache_arch=arm64 ;;' \
  '  *) exit 1 ;;' \
  'esac' \
  'node_path="$RUNNER_TOOL_CACHE/node/$expected_node_version/$toolcache_arch/bin/node"' \
  'test "$("$node_path" --version)" = "v$expected_node_version"' \
  'printf passed > /canary-output/result' \
  >"$entrypoint_canary_directory/run.sh"
chmod 0755 \
  "$entrypoint_canary_directory/dockerd" \
  "$entrypoint_canary_directory/docker" \
  "$entrypoint_canary_directory/findmnt" \
  "$entrypoint_canary_directory/run.sh"
printf '%s\n' overlay2 >"$entrypoint_canary_directory/output/storage-driver"

docker run --rm "${docker_platform_args[@]}" \
  --entrypoint /usr/local/bin/runner-entrypoint \
  -e ACTIONS_RUNNER_INPUT_JITCONFIG=offline-canary \
  -e RUNNERYARD_DEADLINE=2099-01-01T00:00:00Z \
  -e RUNNERYARD_DOCKER_DNS=1.1.1.1,8.8.8.8 \
  -v "$entrypoint_canary_directory/dockerd:/usr/local/sbin/dockerd:ro" \
  -v "$entrypoint_canary_directory/docker:/usr/local/sbin/docker:ro" \
  -v "$entrypoint_canary_directory/findmnt:/usr/bin/findmnt:ro" \
  -v "$entrypoint_canary_directory/run.sh:/home/runner/run.sh:ro" \
  -v "$entrypoint_canary_directory/output:/canary-output" \
  "$runtime_image"
grep -Fx passed "$entrypoint_canary_directory/output/result"
grep -F '"dns":["1.1.1.1","8.8.8.8"]' "$entrypoint_canary_directory/output/daemon.json"

# The runtime itself, not only the repository canary, must fail closed before a
# job starts if Docker falls back to the deep-copy vfs driver.
printf '%s\n' vfs >"$entrypoint_canary_directory/output/storage-driver"
set +e
docker run --rm "${docker_platform_args[@]}" \
  --entrypoint /usr/local/bin/runner-entrypoint \
  -e ACTIONS_RUNNER_INPUT_JITCONFIG=offline-canary \
  -e RUNNERYARD_DEADLINE=2099-01-01T00:00:00Z \
  -v "$entrypoint_canary_directory/dockerd:/usr/local/sbin/dockerd:ro" \
  -v "$entrypoint_canary_directory/docker:/usr/local/sbin/docker:ro" \
  -v "$entrypoint_canary_directory/findmnt:/usr/bin/findmnt:ro" \
  -v "$entrypoint_canary_directory/run.sh:/home/runner/run.sh:ro" \
  -v "$entrypoint_canary_directory/output:/canary-output" \
  "$runtime_image" \
  >"$entrypoint_canary_directory/output/storage-guard.log" 2>&1
storage_guard_status=$?
set -e
test "$storage_guard_status" -eq 78
grep -F "refusing Docker storage driver vfs" "$entrypoint_canary_directory/output/storage-guard.log"

workflow_commands_token=
if [[ ${GITHUB_ACTIONS:-} == true ]]; then
  workflow_commands_token="runneryard-$(openssl rand -hex 32)"
  echo "::stop-commands::$workflow_commands_token"
fi

set +e
docker run --rm --network none "${docker_platform_args[@]}" --entrypoint /bin/bash \
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
