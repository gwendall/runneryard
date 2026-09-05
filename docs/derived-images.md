# Derived worker images

The release image `ghcr.io/gwendall/runneryard:<tag>` is deliberately small:
the runner, Node 22 in the tool cache, Docker, git, `gh`, python3, and
build-essential. Everything a workflow adds on top of that - a package manager
and its store, a second runtime, browsers, `ffmpeg` - is downloaded again by
every job, because a worker is created for one job and destroyed after it.
Measured on a pnpm monorepo on 2026-09-05: four to five minutes of setup per
job (runner wait, checkout, `setup-node` with its cache restore, `pnpm
install`) around three minutes of tests.

A **derived worker image** moves that setup into the image. It is built `FROM`
the release the controller runs, adds what the fleet's workflows need, and is
named in `RUNNER_IMAGE` while the controller keeps running the release image.
Nothing in the controller changes: the Fly adapter passes `RUNNER_IMAGE` to the
Machines API as before, and the worker entrypoint, the tool cache layout, and
the `runner` user come from the base.

## Declare the base

`runneryard doctor` pins `RUNNER_IMAGE` to `[build] image` so a fleet cannot
run workers from one release and a controller from another. A derived image
keeps that discipline by declaring the release it is built from:

```toml
[build]
  image = "ghcr.io/gwendall/runneryard:0.4.4"

[env]
  RUNNER_IMAGE = "registry.fly.io/acme-ci-runners:current"
  RUNNER_IMAGE_BASE = "ghcr.io/gwendall/runneryard:0.4.4"
```

`doctor` passes when `RUNNER_IMAGE_BASE` equals `[build] image`, fails when it
names another release, and fails when `RUNNER_IMAGE` differs from the
controller image with no base declared. Upgrading the fleet means rebuilding
the derived image from the new release and moving `[build] image` and
`RUNNER_IMAGE_BASE` together, as one reviewed commit. The controller ignores
`RUNNER_IMAGE_BASE`; it exists for the reviewer and for `doctor`.

## Build rules

- `FROM` the exact release tag, by tag or digest. Never rebuild the runner,
  Node, or the entrypoints; the canary checks those against the release.
- Install as `root`, then end with `USER runner`. The base sets `HOME`,
  `RUNNER_TOOL_CACHE`, and the entrypoint; keep them.
- Put a runtime the setup actions look for in the tool cache
  (`/opt/hostedtoolcache/<tool>/<version>/<arch>` plus the `.complete` marker)
  so `actions/setup-*` finds it instead of downloading it.
- Warm a package store at a fixed path and export the variable the package
  manager reads (`npm_config_store_dir` for pnpm, `PLAYWRIGHT_BROWSERS_PATH`
  for Playwright browsers). Make it writable by `runner`: a job adds to the
  store when the lockfile moved since the build.
- Keep the image on a registry the workers can pull without a credential:
  on Fly, the worker app's own repository `registry.fly.io/<worker-app>` is
  private to the organization and needs nothing on the Machine.
- Rebuild on a schedule and when the lockfile changes, from a workflow that
  runs on the fleet itself; a warm store that ages still helps, because the
  job only fetches the delta.
- Drop the workflow steps the image made redundant (`cache:` on
  `setup-node`, `apt-get install`, browser installs) or guard them with a
  presence check, and read the timing of the next run: an image that is not
  used by the workflow costs pull time for nothing.

## Example

```dockerfile
ARG BASE=ghcr.io/gwendall/runneryard:0.4.4
FROM ${BASE}
USER root
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg \
  && rm -rf /var/lib/apt/lists/*

# A second runtime, in the tool cache so actions/setup-bun finds it.
ARG BUN_VERSION=1.3.11
RUN set -eux; d=/opt/hostedtoolcache/bun/${BUN_VERSION}/x64; mkdir -p "$d/bin"; \
  curl -fsSL "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-x64.zip" -o /tmp/bun.zip; \
  unzip -q /tmp/bun.zip -d /tmp; mv /tmp/bun-linux-x64/bun "$d/bin/bun"; rm -rf /tmp/bun*; \
  ln -s "$d/bin/bun" /usr/local/bin/bun; touch "/opt/hostedtoolcache/bun/${BUN_VERSION}/x64.complete"

# The package manager and a warm store from the repository's lockfile.
ARG PNPM_VERSION=10.29.3
ENV npm_config_store_dir=/opt/pnpm-store
RUN npm install -g "pnpm@${PNPM_VERSION}" && mkdir -p /opt/pnpm-store
COPY pnpm-lock.yaml /tmp/warm/pnpm-lock.yaml
RUN cd /tmp/warm && pnpm fetch && rm -rf /tmp/warm

# Browsers at a fixed path; the workflow's install step becomes a no-op.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
ARG PLAYWRIGHT_VERSION=1.61.1
RUN npx --yes "playwright@${PLAYWRIGHT_VERSION}" install --with-deps chromium

RUN chown -R runner:docker /opt/pnpm-store /opt/ms-playwright /opt/hostedtoolcache/bun
USER runner
```

Build and push it from the fleet, then point `RUNNER_IMAGE` at it and deploy
the controller with the same `fly deploy --config` as any policy change.
