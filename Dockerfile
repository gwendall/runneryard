ARG GO_IMAGE=golang:1.25.3-bookworm@sha256:4f43b271f9673eb7bd0cb3a49cc17b08d8d6ee110277e26dbacc93c43a5a7793
ARG ACTIONS_RUNNER_IMAGE=ghcr.io/actions/actions-runner:2.337.0@sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4
ARG NODE_VERSION=22.23.2
ARG NODE_IMAGE=node:${NODE_VERSION}-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5
FROM ${GO_IMAGE} AS controller-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
ARG VERSION=dev
ARG COMMIT_SHA=unknown
RUN CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commitSHA=${COMMIT_SHA}" \
  -o /out/runneryard ./cmd/runneryard

FROM ${ACTIONS_RUNNER_IMAGE} AS actions-runner

FROM ${NODE_IMAGE} AS node-runtime

FROM ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
ARG NODE_VERSION
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
  && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl docker-buildx=0.30.1-0ubuntu1~24.04.1 \
    docker-compose-v2=2.40.3+ds1-0ubuntu1~24.04.1 \
    docker.io=29.1.3-0ubuntu3~24.04.2 dumb-init gh git git-lfs iptables jq \
    fuse-overlayfs=1.13-1 libyaml-dev python3 python3-pip sudo unzip util-linux zip \
  && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 1001 runner \
  && useradd --create-home --uid 1001 --gid runner runner \
  && usermod -aG docker,sudo runner \
  && printf '%s\n' '%sudo ALL=(ALL:ALL) NOPASSWD:ALL' >/etc/sudoers.d/runner

COPY --from=actions-runner --chown=runner:runner /home/runner/ /home/runner/
COPY --from=node-runtime /usr/local/ /usr/local/
RUN /home/runner/bin/installdependencies.sh
COPY --from=controller-build /out/runneryard /usr/local/bin/runneryard
COPY controller-entrypoint /usr/local/bin/controller-entrypoint
COPY runner-entrypoint /usr/local/bin/runner-entrypoint
COPY runneryard-job-started.sh /usr/local/bin/runneryard-job-started.sh
RUN chmod 0755 /usr/local/bin/runneryard /usr/local/bin/controller-entrypoint /usr/local/bin/runner-entrypoint /usr/local/bin/runneryard-job-started.sh

ENV HOME=/home/runner
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache
ENV ImageOS=ubuntu24
ENV DOCKER_BUILDKIT=1
RUN set -eux; \
  expected_node_version="v${NODE_VERSION}"; \
  actual_node_version="$(/usr/local/bin/node --version)"; \
  test "${actual_node_version}" = "${expected_node_version}"; \
  case "${TARGETARCH}" in \
    amd64) toolcache_arch=x64 ;; \
    arm64) toolcache_arch=arm64 ;; \
    *) echo "unsupported runner architecture: ${TARGETARCH}" >&2; exit 1 ;; \
  esac; \
  toolcache_dir="/opt/hostedtoolcache/node/${NODE_VERSION}/${toolcache_arch}"; \
  mkdir -p "$(dirname "${toolcache_dir}")"; \
  ln -s /usr/local "${toolcache_dir}"; \
  touch "/opt/hostedtoolcache/node/${NODE_VERSION}/${toolcache_arch}.complete"; \
  chown -R runner:docker /opt/hostedtoolcache /home/runner

USER runner
ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/local/bin/controller-entrypoint"]
