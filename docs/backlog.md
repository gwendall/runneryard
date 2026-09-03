# Backlog

Consolidated on 2026-08-28 against `v0.3.10` (`00cd5c1`) from a code review,
the closed issues, the open items in
[public-release-checklist.md](public-release-checklist.md), and the first
private pilot. Identifiers are stable; open an issue with the identifier when
you start an item.

Delivery status: R1, R2, R3, R4, R5, R6, R7, I8, C2, C8, C10, H3, and O7
shipped in 0.4.0; I1 shipped in 0.3.11 and 0.3.12. The 0.4.1 to 0.4.3
releases shipped W4 and, beyond existing identifiers: `init` can describe an
existing fleet and pins the release in the generated TOML, `doctor` compares
the committed configuration with the live controller, the generated canary
binds the exact release commit and root filesystem, worker departures and
their completions log at `INFO`, pending retirements degrade only after a
grace period, GitHub App secrets are staged on Fly, an unexplained inventory
absence must persist before a replacement is launched, and provider capacity
ceilings are a bounded condition instead of a fatal error. 0.4.4 made the
controller supervise its own GitHub session (a transport failure reopens the
session in-process instead of exiting into the platform's restart backoff),
turned a provider's permanent launch rejection into the same bounded
condition as a capacity ceiling, and taught the Fly adapter every documented
shortage. Next in line: C1,
R10, R13, C4, P1, P3, S3, O1, O3, O4, W1, W6, H5.

How to read it:

- Priority: `P0` before routing anything beyond a canary; `P1` during the
  first production pilot; `P2` larger investments; `P3` someday.
- Effort: `S` under a day, `M` up to a week, `L` two to four weeks, `XL` more.
- Every item names the files it touches so it can become an issue as is.

## Next ten, in order

1. G1 run the production pilot on the 0.4.x line and record the receipts (P1, S)
2. C1 cost receipt in every job log (P1, S)
3. R13 load test before raising the ceiling further (P1, S)
4. R10 nightly integration workflow against a real provider (P1, M)
5. C4 region fallback with a circuit breaker (P1, S)
6. P1 multi-architecture image or narrower documentation (P1, S)
7. O1 troubleshooting guide (P1, S)
8. O4 version, shape, and region visible in the job (P1, S)
9. W1 and W6 status and configuration reference updates (P1, S)
10. H5 release checklist leftovers (P1, M)

## 0. Milestones

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| G1 | Run a 30-day production pilot on low-risk private repositories before routing sustained CI, starting with short jobs and adding longer, Docker-heavy workloads only after a week without intervention. macOS and mobile build jobs stay on hosted runners. | P1 | S to start |
| G2 | Write the pilot exit criteria before starting: zero leaked workers over 30 days, no manual intervention for a week, p95 boot-to-`JobStarted` under 60 s on Fly, and a cost per job at or below the hosted equivalent on the cost receipt (C1). | P1 | S |
| G3 | Write the 1.0 criteria: R1 to R7 shipped, the integration workflow (R10) green for a month, the status schema frozen, and one provider promoted from preview to available only after a public canary receipt. | P1 | S |

## 1. Robustness and correctness

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| R1 | Retry with exponential backoff and a token bucket in `providers/fly` and `providers/hetzner`; classify transient errors (5xx, timeouts, 429) separately from identity errors, and treat transient provider errors as retryable rather than fatal in `controller/scaler.go` and `controller/controller.go`. Add a request budget in front of `scaleset.Client` calls so bursts stay under GitHub's API limits. | P0 | M |
| R2 | `cmd/runneryard/init.go` generates no `[[restart]]` block for the Fly controller; add `policy = "always"`. Compose already uses `unless-stopped`. | P0 | S |
| R3 | `HandleDesiredRunnerCount` launches workers one at a time inside the message loop. Use a bounded `errgroup` for `startWorker` while keeping budget reservation and journal writes serialized, so provider-side waits never delay `JobCompleted` processing. | P0 | S |
| R4 | Idle-worker timeout. Set `ACTIONS_RUNNER_HOOK_JOB_STARTED` to a script that touches a marker; a watchdog in `runner-entrypoint` terminates `run.sh` when the marker is absent after `RUNNER_IDLE_TIMEOUT` (default 10 min). Controller side, retire any worker without `JobStarted` after a dangling timeout (default 25 min). Today an unassigned worker lives the full `RUNNER_MAX_LIFETIME`. | P0 | S |
| R5 | Use `provider.Worker.State` in `reconcile`: retire workers in `off`, `stopped`, or `failed` state immediately instead of waiting for the maximum lifetime, since stopped servers are still billed on some providers. | P1 | S |
| R6 | `CONTROLLER_ID` defaults to `SCALE_SET_NAME`, and `init` derives one worker app per owner. Derive the default from a hash of `GITHUB_CONFIG_URL` and the scale set name, write it explicitly in generated configuration, and make `doctor` check that no other controller shares it in provider inventory, so two fleets in one worker app never share ownership metadata. | P1 | S |
| R7 | On bootstrap failure, print `/home/runner/_diag` and `dockerd.log` to stdout and pause 30 to 60 s before exiting so provider logs capture the cause before `auto_destroy` removes the machine. | P0 | S |
| R8 | Make GitHub runner de-registration configurable: `eager` (current journaled retirement) or `lazy` (skip `RemoveRunner`; GitHub reaps offline JIT runners within 24 h). Eager removal costs one write per worker and introduced the failure mode fixed in #44. | P1 | S |
| R9 | Durable state is three JSON files rewritten wholesale with two `fsync` per event (`usage_budget.go`, `retirement_queue.go`, `status.go`). Move to SQLite (`modernc.org/sqlite`, no cgo) or an append-only journal with compaction, with a schema migration policy. | P2 | M |
| R10 | Nightly integration workflow (`workflow_dispatch` plus schedule) against a dedicated Fly test organization: deploy the controller, run the canary, assert zero workers and zero GitHub registrations after completion. Gate releases on it. | P1 | M |
| R11 | Property and fuzz tests for the ledger invariants (`validate`, `prune`, `reserve`/`settle`/`forfeit` sequences) and for the retirement queue state machine. | P2 | S |
| R12 | Document and test the behavior when two controllers start for one scale set (GitHub refuses the second session); add a `doctor` check that the session is free before deployment. | P2 | S |
| R13 | Load test before raising `MAX_RUNNERS` beyond a few dozen: a `workflow_dispatch` matrix of 150 one-minute jobs, watched through `status`, provider logs, and the provider bill; document the step-up ladder (24, 60, 100) and the provider capacity and API rate limits observed. | P1 | S |

## 2. Cost and worker lifecycle

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| C1 | Cost receipt in every job log: an `ACTIONS_RUNNER_HOOK_JOB_COMPLETED` script prints provider, shape, region, duration, estimated cost, and the hosted-runner equivalent. Embed a price table per provider, region, and shape in the binary with a `runneryard price` subcommand; report billed hours where the provider bills by the hour. | P1 | S |
| C2 | Push alerts: a generic webhook (Slack-compatible) fired when `status.health` becomes `degraded`, when the budget refuses admission, and when orphan candidates appear; optional daily summary. The state machine exists in `controller/status.go`; only the emitter is missing. | P1 | S |
| C3 | Hetzner billing model. Hetzner bills every started hour, so one server per short job can exceed the hosted-runner price. Either reuse a server inside its billed hour (keep it up to ~55 min after a job and re-lease it with a fresh JIT), or document Hetzner as suited to long jobs, and say which in the provider guide. | P1 | M |
| C4 | Region fallback with a circuit breaker for Fly capacity errors: `RUNNER_FLY_REGION=cdg+ams`, with count, window, and recovery parameters (for example two failures in 15 min move launches to the next region for 30 min). | P1 | S |
| C5 | Stopped standby pool on Fly. A stopped Machine costs only its rootfs and starts in one to two seconds with the image already on the host. The JIT configuration is single-use, so update the Machine `env` while stopped, then start it. Replace `MIN_RUNNERS` with `hot` and `stopped` counts and an optional business-hours schedule. | P2 | M |
| C6 | `runneryard budget status` and `budget raise --to <duration> --confirm` so operators never edit the ledger by hand; monthly usage summary. | P2 | S |
| C7 | Worker shape per scale set (after M1) so short jobs can use `shared` CPUs and long jobs `performance` CPUs. | P2 | S |
| C8 | Settle usage from the provider's actual worker lifetime (creation to machine stop, read from provider events) instead of the arrival time of `JobCompleted`; with one-job workers that self-destroy, the ledger charges the message latency and overcounts short jobs. Also expose the budget horizon (hours remaining at the current burn rate) in `status`. | P0 | S |
| C9 | Opt-in bounded worker reuse for trusted private repositories: after `JobCompleted`, hand the same machine a fresh JIT configuration and run the next job, up to N jobs or T minutes, with a cleanup step between jobs (`_work`, containers, stray processes); then retire it. Removes boot latency and keeps package and Docker caches warm. Never the default; documented as an isolation trade-off. | P2 | L |
| C10 | Allow lowering `RUNNER_MAX_LIFETIME` on a fleet with active reservations: today `validate` rejects a ledger whose active entries exceed the new reservation, so the controller refuses to start. Migrate active entries safely on startup and document it. | P1 | S |

## 3. Provider adapters

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| P1 | The GHCR image is `linux/amd64` only (`release.yml` builds with `--platform linux/amd64`) while `docs/security.md` describes the toolcache on x64 and arm64. Build multi-platform with `buildx` and run the offline toolcache canary on both, or narrow the documentation. | P1 | S |
| P2 | Deliver the JIT configuration through a short-lived presigned object (Tigris on Fly, Object Storage on Hetzner) instead of environment or instance user data: the controller writes the lease, passes the URL, and deletes the object on `JobStarted`. Instance user data persists in provider metadata for the VM lifetime, so this is the stronger boundary. | P2 | M |
| P3 | Complete a public Hetzner canary in a real project, record the receipt, and only then move the adapter from "preview" to "available". | P1 | S |
| P4 | Hetzner CAX (Ampere) support once P1 ships an arm64 image. | P2 | S |
| P5 | Adapter conformance suite (`provider/providertest`): a reusable Go test that proves the eight required capabilities from `adapter-contract.md` against any `Compute` implementation, so third-party adapters can self-certify. | P2 | M |
| P6 | Third adapter: decide after the pilot between a large general-purpose cloud and a European provider, based on where pilot users run. | P2 | XL |
| P7 | Fly: document `auto_destroy` and restart semantics precisely, and use stop instead of destroy for pool members (C5). | P2 | S |

## 4. Performance and image

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| I1 | Done in 0.3.11 and 0.3.12: `runner-entrypoint` now selects `fuse-overlayfs` on layered provider roots and keeps BuildKit enabled. Remaining: measure build throughput against a hosted runner on a representative Docker job and record it on the benchmark page (I5). | P1 | S |
| I2 | Image tiers with explicit names: `minimal` (current: runner, Node 22, Docker, git, gh, python3, build-essential) and `full` built from the upstream `actions/runner-images` install scripts, with a lock file pinning the upstream revision, a scheduled rebuild, and a provenance manifest. `RUNNER_IMAGE` selects the tier. | P2 | L |
| I3 | Prewarm common workflow needs: Node 20/22/24 LTS majors in the toolcache (a floating `node-version` currently downloads a newer patch), Python 3.12 and 3.13 for `actions/setup-python`, pnpm, and an optional Playwright browsers layer. | P1 | S |
| I4 | Measure and publish cold and warm boot on Fly per region and image size; consider a slimmer base for `minimal`. | P1 | S |
| I5 | Benchmark page: boot latency, job duration, and cost per job against `ubuntu-latest` on Fly `performance-2x`, Fly `shared-4x`, and Hetzner CPX32, from a reproducible workflow committed in this repository. | P1 | M |
| I6 | Docker pull-through mirror: `registry:2` in proxy mode backed by Tigris, added to the daemon's `registry-mirrors`; avoids registry rate limits and speeds up every `docker run` and `services:` job. | P2 | M |
| I7 | Optional tmpfs mode (`/tmp`, `/home/runner`, `/var/lib/docker` in memory) for I/O-bound jobs on large-memory shapes. | P3 | S |
| I8 | Start the runner without waiting for Docker: `runner-entrypoint` currently blocks on `docker info` before `run.sh`; start both in parallel so registration overlaps the daemon startup, and only fail the job if Docker is still absent when a step needs it. | P0 | S |

## 5. Security

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| S1 | Publish GitHub artifact attestations (`actions/attest-build-provenance`) for release binaries and verify them in `packages/cli/bin/runneryard.mjs` in addition to the checksum file. | P2 | M |
| S2 | Sign the container image (cosign) and attach an SBOM; verify the digest in `verify-runtime-toolcache.sh`. | P2 | S |
| S3 | Threat model additions in `docs/security.md`: the runner user has passwordless `sudo` and the Docker socket by design (hosted-runner parity), job code can read the already-consumed JIT from the process environment, provider metadata notes per adapter, and the P2 handoff once it ships. | P1 | S |
| S4 | `doctor` check that the GitHub App holds only the required permission (`administration: write` for repositories, `organization_self_hosted_runners: write` for organizations) and no webhook. | P2 | S |
| S5 | Run `govulncheck`, `staticcheck`, and `gosec` in `ci.yml`. | P1 | S |
| S6 | Confirm branch protection requiring CI, private vulnerability reporting, secret scanning, and push protection are enabled on the repository. | P1 | S |

## 6. Observability and operations

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| O1 | `docs/troubleshooting.md`: "job stays queued", "runner never registers", "Fly capacity unavailable", "budget exhausted", "two controllers on one scale set", each with the exact diagnostic command and the safe next action. | P1 | S |
| O2 | `runneryard logs <job-url or runner-name>`: map a job to its runner name, then to the Fly Machine (`fly logs --instance`) or Hetzner console output, with a `--full` archive option. | P2 | M |
| O3 | Document `fly ssh console --app <workers> --select` as the remote-access path, and add `RUNNER_DEBUG_HOLD` (keep a failed worker alive N minutes, bounded by the lease) for interactive debugging. | P1 | S |
| O4 | Make the runner name or labels carry version, shape, and region (`runneryard@0.3.10 fly performance-2x cdg`) so "Set up job" shows them to every developer. | P1 | S |
| O5 | Optional OTLP push exporter for the status snapshot (no inbound port), plus a Prometheus textfile writer beside `status.json`. | P2 | M |
| O6 | Per-scale-set breakdown in `status` once M1 lands. | P2 | S |
| O7 | Upgrade and support policy: minor and patch releases without downtime, the last two minor versions supported, a `CHANGELOG.md` per release, and a documented rollback (pin the previous image, preserve the volume). | P1 | S |

## 7. Developer experience and CLI

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| D1 | `doctor` additions: `CONTROLLER_ID` uniqueness (R6), image architecture matching the provider shape (P1), ledger present on the volume, App permission minimality (S4), no second controller session (R12). | P1 | S |
| D2 | `init` should update generated files with a reviewable diff instead of refusing or overwriting with `--force`. | P2 | S |
| D3 | Homebrew tap and `mise` installation for the binary, version-pinned per fleet. | P3 | S |
| D4 | `route` support for organization variables and for more than one variable (Linux and arm64 labels). | P2 | S |
| D5 | Windows support in the npm launcher, or an explicit unsupported message with the release binary path. | P3 | S |
| D6 | Schema and linter for `runneryard.yml` (after M2), with SARIF output for CI. | P3 | M |
| D7 | `runneryard route adopt`: open a pull request that rewrites hard-coded `runs-on: ubuntu-latest` into the variable expression across workflows, plus a CI check that flags new workflows with a hard-coded hosted label. | P1 | M |
| D8 | Opt-in one-way automatic fallback: when status is `degraded` for longer than a threshold or the budget refuses admission, the controller removes the routing variable itself and notifies; it never re-enables routing on its own. Requires the App to hold `actions: variables` write. | P1 | M |
| D9 | Per-workflow rollout: `route enable --workflows ci.yml` writes a dedicated variable per workflow class so short jobs migrate first and deployments last. | P2 | S |

## 8. Multiple scale sets and configuration

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| M1 | Several scale sets per controller: one listener goroutine per scale set, one shape (`cpu`, `ram`, `image`, `region`, `max`) per scale set, one shared budget. This gives `runs-on: acme-linux-small` and `acme-linux-large` without a second controller and without a per-job label language. | P2 | L |
| M2 | `.github/runneryard.yml` with `runners`, `images`, `pools`, `admins`, and `_extends` to an organization `.github-private` repository, read by the controller with a CUE or JSON schema. Only after M1, otherwise the file has nothing to configure. | P2 | M |
| M3 | Organization fleets: runner-group documentation, `doctor` checks, and a canary at organization scope. | P2 | S |

## 9. Cache

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| K1 | S3-compatible cache next to the workers (Tigris on Fly, Object Storage on Hetzner) with an `actions/cache`-compatible action that targets any S3 endpoint. This requires one credential on the worker; scope it to the cache bucket only and state the trade-off against the "one-time credential only" model in `docs/security.md`. | P2 | M |
| K2 | Short-lived per-lease cache credentials if the object store supports them; otherwise document K1 as the model. | P3 | M |
| K3 | Action-free cache: a local proxy on the worker speaking the Actions cache protocol, backed by the same bucket. Separate project. | P3 | XL |
| K4 | Persistent build volumes on Fly (volume fork or snapshot per lineage) for `node_modules`, Go and Rust caches, and BuildKit state. Evaluate after K1. | P3 | XL |

## 10. Website and documentation

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| W1 | Status on `apps/web` that matches the docs: pre-1.0, Fly available, Hetzner preview. | P1 | S |
| W2 | Benchmark and cost page from I5. | P1 | S |
| W3 | Alternatives page: when to prefer hosted runners, a Kubernetes-based controller, or a managed runner service instead of RunnerYard. | P2 | S |
| W4 | Replace the two em-dashes (`cmd/runneryard/status.go:37`, `apps/web/app/security/page.tsx:55`) with plain punctuation. | P1 | S |
| W5 | Changelog page fed by GitHub releases. | P2 | S |
| W6 | `docs/configuration.md`: list every variable including `RUNNER_STATUS_FILE`, the retirement file, and all new ones; version the docs per release. | P1 | S |
| W7 | Lighthouse accessibility, performance, best-practices, and SEO pass on the deployed site. | P2 | S |

## 11. Release engineering and repository hygiene

| Id | Item | Priority | Effort |
| --- | --- | --- | --- |
| H1 | Batch releases weekly, keep pre-1.0 semver, and publish the 1.0 criteria from G3. | P1 | S |
| H2 | Remove stale worktrees and their squash-merged `agent/*` and `release/*` branches; keep only active work. | P1 | S |
| H3 | Add `govulncheck`, `staticcheck`, and `gosec` to `ci.yml` (S5). | P1 | S |
| H4 | Run the offline toolcache canary on arm64 once P1 ships. | P2 | S |
| H5 | Release checklist items still to verify: branch protection requiring CI, private vulnerability reporting, secret scanning and push protection, `npx runneryard@<version> version` from an empty cache on macOS and Linux, the Hetzner canary (P3), a rehearsed budget exhaustion, a rehearsed outboarding. Verified: GHCR anonymous pull, npm trusted publishing with provenance, four binary targets with checksums, immutable releases. | P1 | M |
| H6 | Turn this file into GitHub issues with the same identifiers once G1 starts, and keep the file as the roadmap index. | P1 | S |

## Done (for context)

Closed issues: #11 no-copy GitHub App onboarding, #12 hosted-runner failover
commands, #13 fleet status receipts, #14 first-run path, #19 to #31 releases
0.3.0 to 0.3.4, #21 App package in the release image, #23 browser fallback
URLs, #27 and #47 prewarmed Node toolcache, #33 GitHub registration garbage
collection, #36 canonical public onboarding, #39 unknown worker state after
recovery, #42 performance CPU defaults, #44 deferred busy-runner retirement.
