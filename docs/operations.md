# Operations

## Healthy lifecycle

For each assigned job, structured logs should show `worker created`, `job
started`, and `job completed`. Provider inventory should return to
`MIN_RUNNERS`, normally zero.

The controller reconciles inventory before every desired-count change. It
adopts managed workers created by the same controller after a restart, removes
local records for disappeared workers, and destroys workers older than
`RUNNER_MAX_LIFETIME`.

Desired-count updates only scale up. They never opportunistically delete an
apparently idle JIT runner: GitHub may already be assigning that runner while
the corresponding `JobStarted` message is still in flight. One-job workers are
removed synchronously on `JobCompleted`; unused workers remain bounded by their
provider lease and `RUNNER_MAX_LIFETIME`.

Worker retirement converges both control planes: provider compute is confirmed
absent, then the matching GitHub runner registration is removed. The intent is
written first to a durable `retirements.json` journal beside the budget ledger,
so a controller restart or transient GitHub API failure cannot lose cleanup.
Removal is restricted to the journaled GitHub registration ID and scale-set ID;
the matching lease and registration proof also travel in provider metadata.
If provider ownership is ambiguous, cleanup fails closed and remains pending.
GitHub can briefly reject removal with `job still running` after an ephemeral
worker has exited, especially when a workflow is cancelled or superseded. That
typed response is the only deferred cleanup case: the listener stays available,
status remains degraded with a pending retirement, and reconciliation retries
the same immutable registration ID. Identity or provider ambiguity remains a
hard error.

## Safe upgrades

Pin the runtime image to a release version. Stop the old controller cleanly and
allow its GitHub message session to close before starting its replacement; two
listeners cannot own one scale set concurrently. Existing workers are preserved
for the successor and can finish because their entrypoint is self-contained.
The replacement adopts them from provider inventory. Preserve the durable
volume, `CONTROLLER_ID`, ledger, and retirement journal. Never run `budget init`
during an upgrade.

Treat a tag as a candidate until its release checksums and image digest are
published. On Fly, the pinned deployment image is also the worker image. On
Hetzner, update both independent pins to the same release: `RUNNER_IMAGE` in
`controller.env` for workers and `image` in Compose for the controller. Replace
the controller, verify `runneryard status` reports the intended controller
version and commit, and set the generated canary's
`RUNNERYARD_EXPECTED_VERSION` to the same release so its job proves the worker
image. Run the canary and only then recover broader workflow routing with an
explicit receipt:

```sh
npx runneryard route enable \
  --github https://github.com/acme/widgets \
  --label acme-linux \
  --confirm-canary
```

## Capacity

Start with `MIN_RUNNERS=0` and a small `MAX_RUNNERS`. Queue time is safer than an
unbounded cloud bill. Increase the ceiling from observed job concurrency, not
PR count. Split workload classes into separate scale sets when they need
different trust boundaries or machine shapes. Keep the live ceiling in
operator-reviewed provider configuration; never let repository code or a pull
request change it. See the [configuration reference](configuration.md) for the
budget admission formula and provider-specific shapes.

## Runtime tooling and toolcache

Workers include the Docker Buildx client and enable BuildKit for job steps.
Their nested daemon selects the VM's native storage driver instead of forcing
Docker's deep-copy `vfs` fallback. A job log that reports the legacy builder,
or `docker info` reporting `vfs` on a VM-backed provider, means the runtime
image or host is outside the qualified path. Stop routing Docker-heavy jobs
until a canary has verified `docker buildx version` and the provider's native
driver; repeated legacy builds can consume the ephemeral rootfs before tests
start.

Docker Desktop is not a substitute for that provider canary: a privileged
Docker daemon nested inside Desktop can reject overlay-on-overlay mounts even
when the same image correctly uses the native driver on a VM-backed worker.
The generated repository canary performs the provider proof: it checks the
prewarmed Node path, invokes pinned `actions/setup-node`, rejects `vfs`, and
builds and runs a digest-pinned minimal image through Buildx.

The release image publishes its digest-pinned Node patch through the GitHub
runner toolcache. Pin `actions/setup-node` to that exact patch when fast,
reproducible bootstrap matters. A floating request such as `node-version: 22`
may resolve to a newer patch after the image was published; `setup-node` then
correctly downloads it for every disposable worker instead of using the local
toolcache. That is a version mismatch, not a cache miss.

Check the release Dockerfile for the prewarmed patch, or read it from a worker
with `node --version`. The entrypoint explicitly forwards the qualified
`RUNNER_TOOL_CACHE` into the unprivileged Actions runner; if an exact
`actions/setup-node` request still says it is downloading that patch, treat it
as an image regression rather than accepting the cold download. Upgrade the
RunnerYard image before moving repository workflows to a newer patch. Keep the
workflow and image changes independently reviewable: repository code must not
be able to select an untrusted worker image.

Published worker tags are multi-architecture manifests. Release qualification
builds and runs the offline entrypoint/toolcache canary separately on amd64 and
arm64 before composing the immutable manifest.

## Fleet status

The controller writes `/var/lib/runneryard/status.json` atomically with mode
`0600` and refreshes it every 30 seconds. Read it inside the authenticated
controller boundary:

```sh
runneryard status
runneryard status --json
```

On Fly, no public status service is required:

```sh
fly ssh console --app acme-ci-controller \
  --command '/usr/local/bin/runneryard status --json'
```

On a Docker controller such as Hetzner:

```sh
docker compose -f .runneryard/hetzner.controller.compose.yml \
  exec controller /usr/local/bin/runneryard status --json
```

Healthy interpretation:

- `ready` with a recent `updated_at` means the controller heartbeat is alive.
- `actual + starting == maximum` is normal under load but means capacity is
  saturated; compare assigned jobs and assignment latency before raising the
  cap.
- provider create latency measures infrastructure boot. Assignment latency
  measures the separate interval from worker creation to GitHub job start.
- `busy + idle + unknown == actual`; these three counters are mutually
  exclusive observations in status schema version 2.
- idle means a created JIT worker has not emitted `JobStarted`; it is not safe
  to delete opportunistically because assignment may already be in flight.
- unknown means the current controller adopted a worker after restart and has
  not observed that worker's lifecycle yet. It is neither claimed busy nor
  safe to delete: GitHub may have assigned it before the old session closed.
  The next `JobStarted` makes it busy; completion or the maximum lifetime
  retires it normally.
- any orphan candidate, pending retirement, or stable failure reason makes
  health `degraded`. A pending retirement is retried during reconciliation and
  must return to zero after provider and GitHub cleanup recover.
- budget used is settled compute, reserved is worst-case time held by active
  workers, and remaining is admission headroom. `usage_budget_exhausted` means
  new jobs intentionally stay queued.

The schema contains aggregate counters only. It never writes job IDs,
repository payloads, runner names, JIT configuration, tokens, or secrets. The
latency aggregates have fixed fields and no user-controlled metric labels.

## Hard usage budget

`RUNNER_USAGE_BUDGET` is the maximum worker runtime inside the rolling
`RUNNER_BUDGET_WINDOW`. The default scaffold allows 10,000 runner-minutes per
30 days. The controller reserves a full `RUNNER_MAX_LIFETIME` before launch,
stores the ledger at `RUNNER_BUDGET_FILE`, and refunds unused time only after a
confirmed worker deletion. The job deadline leaves 30 seconds of that
reservation for forced shutdown. Keep the file on a durable volume. Missing,
corrupt, or unwritable state stops new compute rather than resetting the cap.
The separate `runneryard budget init --file PATH` command bootstraps a new
volume and refuses to overwrite a ledger. Never automate it on controller
restart.

The retirement journal is controller state, not a cache. Keep it on the same
durable private volume as the budget ledger. Missing state is initialized once;
corrupt or unwritable state stops retirement before provider deletion. Do not
edit or reset it while a fleet is active. Upgrading from a non-empty legacy
journal is intentionally refused: let the previous controller drain it first,
then upgrade with an empty fleet and journal.

Queued jobs are the intended failure mode. Raise the budget from observed
qualified workload, and account separately for the small always-on controller,
storage, and network costs.

## Incident response

If workers leak, stop new routing first:

```sh
npx runneryard route disable --github https://github.com/acme/widgets
```

This removes `CI_LINUX_RUNNER`, so workflows using
`${{ vars.CI_LINUX_RUNNER || 'ubuntu-latest' }}` select the hosted runner. The
expression is a configuration fallback, not an availability detector. Verify
the receipt with `runneryard route status`, then stop the controller. Inventory
the provider using the controller metadata; do not bulk-delete foreign machines.
Workers are intentionally preserved across controller shutdown and remain
bounded by their provider lease. Revoke the provider token if ownership is
uncertain.

If the controller is unavailable, jobs targeting its label queue safely on
GitHub. Run `route disable` to restore the hosted fallback, subject to GitHub
billing availability. The command is idempotent, supports `--dry-run`, uses the
existing local `gh` identity, and never asks for or prints its token.
