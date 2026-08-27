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

Worker deletion is idempotent and confirmed against provider inventory when a
delete request returns an ambiguous transport error. If the worker is already
absent, the controller settles its lease and keeps serving the scale-set
listener. If inventory still contains the worker, cleanup fails closed and the
local record is retained for reconciliation.

## Safe upgrades

Pin the runtime image to a release version. Stop the old controller cleanly and
allow its GitHub message session to close before starting its replacement; two
listeners cannot own one scale set concurrently. Existing workers are preserved
for the successor and can finish because their entrypoint is self-contained.
The replacement adopts them from provider inventory. Run the canary and only
then update broader workflow routing.

## Capacity

Start with `MIN_RUNNERS=0` and a small `MAX_RUNNERS`. Queue time is safer than an
unbounded cloud bill. Increase the ceiling from observed job concurrency, not
PR count. Split workload classes into separate scale sets when they need
different trust boundaries or machine shapes.

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

Queued jobs are the intended failure mode. Raise the budget from observed
qualified workload, and account separately for the small always-on controller,
storage, and network costs.

## Incident response

If workers leak, stop new routing, then stop the controller. Inventory the
provider using the controller metadata; do not bulk-delete foreign machines.
Workers are intentionally preserved across controller shutdown and remain
bounded by their provider lease. Revoke the provider token if ownership is
uncertain.

If the controller is unavailable, jobs targeting its label queue safely on
GitHub. Set the repository runner variable back to `ubuntu-latest` to restore a
hosted fallback, subject to GitHub billing availability.
