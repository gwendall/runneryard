# Changelog

RunnerYard is pre-1.0 and follows semantic versioning: patch and minor releases
are drop-in upgrades for a running fleet, and anything that changes an
operator-facing schema or a trust boundary is called out here first.

## Unreleased

- `runneryard init` can describe a fleet that already exists: `--controller-app`,
  `--worker-app`, `--controller-id`, `--rootfs-gb`, and `--usage-budget` replace
  the derived defaults. The generated Fly TOML pins the release in
  `[build] image` and `RUNNER_IMAGE`, so `fly deploy --config` needs no
  `--image` argument, and `controller.env.example` now lists only the secrets
  that belong in the Fly secret store.
- The generated canary binds the worker to the exact release commit when the
  generating binary is a release, checks that the root filesystem honours
  `RUNNER_ROOTFS_GB`, verifies `RUNNER_ENVIRONMENT`, and runs every step under
  `set -euo pipefail`.
- `runneryard doctor` compares the committed Fly TOML (`--config`, defaulting
  to `.runneryard/fly.controller.toml`) with every live controller Machine and
  fails on environment, image, restart-policy, or mount drift, on divergent
  image pins inside the file, and on more than one controller Machine.

## 0.4.0 (2026-08-30)

- Compute adapters retry throttling, provider-side errors, and transport
  failures with bounded backoff and request pacing; a create is only repeated
  after inventory proves the lease has no worker. Exhausted retries keep the
  GitHub session open and report `degraded` instead of stopping the
  controller. New settings: `PROVIDER_RETRY_ATTEMPTS`, `PROVIDER_RATE_LIMIT`,
  `GITHUB_API_RATE_LIMIT`.
- Scale-up bursts launch workers concurrently, bounded by
  `RUNNER_LAUNCH_CONCURRENCY`.
- The generated Fly controller TOML pins `[[restart]] policy = "always"`.
- The worker starts the GitHub runner alongside the Docker daemon and prints
  runner and Docker diagnostics, then holds briefly, before exiting on failure.
- Workers that never receive a job release themselves after
  `RUNNER_IDLE_TIMEOUT`; the controller retires its own idle workers after
  `RUNNER_DANGLING_TIMEOUT` and retires stopped workers immediately.
- Completed jobs are charged from worker creation to GitHub's job finish time
  plus a teardown grace; the status snapshot reports `burn_seconds_per_day`
  and `horizon_seconds`.
- Lowering `RUNNER_MAX_LIFETIME` on a fleet with active reservations no longer
  prevents the controller from starting.
- `ALERT_WEBHOOK_URL` posts health transitions and hourly reminders to a
  Slack-compatible webhook.
- `runneryard init` writes a controller identity that is unique per GitHub
  target; the controller warns when `CONTROLLER_ID` is left unset.
- CI runs staticcheck, gosec, and govulncheck.
- The Go toolchain and the release image move to Go 1.25.14 for the standard
  library security fixes.

## 0.3.15 (2026-08-29)

- Workers include Docker Compose.

## 0.3.14 (2026-08-29)

- The Fly adapter honors the configured worker rootfs size.

## 0.3.13 (2026-08-28)

- Nested Docker DNS on Fly is explicit and validated at the worker boundary.

## 0.3.12 (2026-08-28)

- BuildKit works on layered provider roots through fuse-overlayfs.

## 0.3.11 (2026-08-28)

- The worker image preserves the toolcache and enables BuildKit.

## 0.3.10 (2026-08-28)

- The prewarmed Node 22 toolcache patch is refreshed.

## 0.3.9 (2026-08-28)

- Busy runner retirement is deferred instead of crash-looping when GitHub
  still considers the job running.

## 0.3.8 (2026-08-28)

- Fly workers default to performance CPUs for sustained CI workloads.

## 0.3.7 (2026-08-28)

- Workers recovered after a controller restart report as `unknown` until
  their lifecycle is observed; status schema version 2.

## 0.3.6 (2026-08-28)

- Public onboarding and operations documentation are canonical.

## 0.3.5 (2026-08-28)

- Offline GitHub runner registrations are garbage-collected through a
  durable retirement journal.

## 0.3.4 (2026-08-27)

- The offline release canary is isolated from workflow commands.

## 0.3.3 (2026-08-27)

- The pinned Node toolcache is prewarmed in the worker image.

## 0.3.2 (2026-08-27)

- Browser fallback URLs are always printed during GitHub App setup.

## 0.3.1 (2026-08-27)

- Release images include every internal package.

## 0.3.0 (2026-08-27)

- Guided first-run path to a green canary, private fleet status receipts,
  explicit runner failover commands, and no-copy GitHub App onboarding.

## 0.2.x and 0.1.x (2026-08-27)

- Initial public releases: Fly Machines adapter, Hetzner preview adapter,
  durable usage budget, npm launcher with checksum verification.
