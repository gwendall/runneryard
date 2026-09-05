# Changelog

RunnerYard is pre-1.0 and follows semantic versioning: patch and minor releases
are drop-in upgrades for a running fleet, and anything that changes an
operator-facing schema or a trust boundary is called out here first.

## Unreleased

- Derived worker images. A fleet can run its workers from an image built
  `FROM` the release the controller runs - the release plus whatever its
  workflows install on every job (a package manager and its warm store, a
  second runtime, browsers, `ffmpeg`) - by naming that image in
  `RUNNER_IMAGE` and declaring the release in the new `RUNNER_IMAGE_BASE`.
  `doctor` keeps the pin discipline: it passes when the declared base equals
  `[build] image`, fails when the base is another release, and still fails
  when `RUNNER_IMAGE` differs from the controller image with no base
  declared. The controller ignores the key. Guide: `docs/derived-images.md`.
  Motivation, measured on a pnpm monorepo on 2026-09-05: four to five
  minutes of setup per job around three minutes of tests.

## 0.4.4 (2026-09-04)

- The controller supervises its own GitHub scale-set session. A transport
  failure - a message poll, an acknowledgement, a job acquisition, or the
  session request itself - used to end the process and leave recovery to the
  platform's restart policy; on Fly that backoff reached fifteen minutes on
  2026-09-03 with twenty-three runs queued behind a dead 0.4.2 controller.
  The session is now closed and reopened in-process with a backoff of 5 s
  doubling to 2 min, against the same scaler, so worker state, the launch
  gate, and the budget survive. Status reports `github_session_restarting`
  between sessions. Only the scaler's fail-closed handler errors (identity,
  state, ledger) or cancellation still end the controller.
- A provider's permanent rejection of a launch request - an unusable image
  or shape, a region without the resources, a name conflict - is a bounded
  condition like a capacity ceiling instead of a fatal error. The controller
  proves that no worker carries the lease, refunds the reservation, removes
  the JIT registration, and probes again after the same doubling backoff.
  Status carries `launch.provider_rejection` (`fly_status_422` and the like,
  never a raw payload), `launch.rejections`, and `launch.retry_at`; health
  reports `provider_launch_rejected` with an actionable alert. A `401` or
  `403` from the provider still fails closed.
- The Fly adapter classifies every documented shortage as capacity, not only
  the organization machine limit: `fly_insufficient_resources` (a region that
  could not reserve memory or CPU, a volume without room) and
  `fly_placement_unavailable` (no host could place the Machine) join
  `fly_machine_limit`. Validation errors stay permanent.
- After a restart, job messages about runners the new process never created
  are logged at `INFO` for the first hour instead of `WARN`: they are the
  predecessor's workers finishing their jobs, and forty warnings for routine
  successes hid the one that mattered.

## 0.4.3 (2026-08-31)

- Provider quota exhaustion is a bounded capacity condition instead of a
  fatal controller error. Fly machine-limit responses keep the GitHub session
  alive, preserve completions and replacements below the proven ceiling, and
  use one exponentially backed-off probe above it. Status adds configured and
  effective capacity, rejection count/code, and the next probe time; alerts
  give the operator the quota or `MAX_RUNNERS` action without exposing a raw
  provider response.

## 0.4.2 (2026-08-30)

- Reconciliation now requires an unexplained provider-inventory absence to
  remain continuous for 30 seconds before forgetting a worker. Reappearance
  or `JobStarted` clears the observation, preventing an eventually consistent
  Fly snapshot from spawning a duplicate replacement while assignment is in
  flight; explicit stopped states and lifecycle deadlines still retire
  immediately.
- `auth github create` and `auth github import` stage the App secrets on Fly
  (`fly secrets import --stage`) instead of redeploying immediately, and both
  sinks print how to bring the credential into service. A controller that
  still authenticates with `GITHUB_TOKEN` is no longer restarted into a
  refusal to start with both credential sets; the operator removes the token
  and the App goes live in that single restart.

## 0.4.1 (2026-08-30)

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
- A busy worker that leaves provider inventory before GitHub reports its job
  finished, and the completion message that follows, are logged at `INFO`
  with the job result; an idle worker that released itself after
  `RUNNER_IDLE_TIMEOUT` and an adopted worker are explained the same way.
  Only a worker that vanished before starting a job and a completion for a
  runner the controller never knew still warn.
- The generated canary keeps a reviewable version comment next to the pinned
  `actions/setup-node` commit.
- A pending runner retirement degrades the fleet only once it has stayed
  pending for longer than fifteen minutes. GitHub keeps a runner registered,
  and refuses its removal as "job still running", for minutes after the job
  completed, so a fresh deferral is now reported as pending without an alert.
  The retirement journal records `requested_at` and the status snapshot adds
  `overdue_retirements`; both are additive.

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
