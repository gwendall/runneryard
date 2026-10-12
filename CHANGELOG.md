# Changelog

RunnerYard is pre-1.0 and follows semantic versioning: patch and minor releases
are drop-in upgrades for a running fleet, and anything that changes an
operator-facing schema or a trust boundary is called out here first.

## 0.5.6 (2026-10-12)

- A pool slot start that answers late is kept. Why: a start that outlasted the
  controller's 90 s timeout, or whose SSH session ended before the answer, was
  counted as a failed launch and its slot torn down, although the host agent had
  started the runner. The host agent now keeps a start running for up to two
  minutes, and the controller lists the host again for up to 30 s: a slot that runs
  a container with the same lease, runner name and controller ID is committed as a
  normal launch and logged at INFO. Anything else, or a listing failure, keeps the
  ambiguous-start path as it was.
- Routine stop logs are quieter. Why: a runner exits after its one job, so its slot
  is listed stopped before GitHub's completion arrives, and every pool job logged a
  warning about a stopped worker and then one about an unknown runner. The
  controller now logs that retirement at INFO, remembers the worker, and matches the
  start and completion messages that follow. A stopped worker whose completion has
  not arrived after five minutes is reported once at WARN, and a failed or unknown
  stopped worker still warns at once.
- The pool status reports occupancy and cost. Each host measures, from the moment
  this controller process first sees it, its billed running time, its ready time,
  and the slot time that held a runner; the pool adds them up, returned hosts
  included, as `occupancy` (busy slot time over paid slot time) and
  `ready_occupancy` (over the slot time of ready hosts only). `measured_cost` covers
  the same time, and `ephemeral_equivalent` prices the busy slot time at the worker
  price of the first tier that knows it, through a new optional `provider.Pricer`
  that the Fly adapter implements. Why: an operator could not tell whether a second
  host was busy enough to pay for itself. The figures live in memory and start again
  at zero when the controller restarts (`measured_since`); `runneryard status` prints
  one line per pool and one per host. A Hetzner ephemeral tier bills per started
  hour and has no comparison.
- The controller alerts on pool conditions. Why: a pool problem, such as a host
  that stopped answering or a host source whose token was revoked, only showed in
  the log, and a log watcher that dies on an expired token stays silent. The pools
  now report nine conditions (`host_source_unavailable`, `image_tag_unresolved`,
  `host_launch_failing`, `host_boot_failed`, `host_unreachable`, `host_not_serving`,
  `host_refusing`, `foreign_slot`, `pool_overflowing`), read every five seconds. Each
  one is logged when it appears and when it clears, listed under
  `compute.pools[].conditions` in `status.json`, and with `ALERT_WEBHOOK_URL` sent as
  one message when it appears, one when it clears, and a reminder at most hourly.
  A message says what is wrong and what to do, and never carries provider error
  text, addresses, tokens or JIT configuration. The normal life of a host raises
  nothing. `docs/operations.md` lists when each condition is raised and cleared.

## 0.5.5 (2026-10-12)

- A pool starts a speculative runner in a free slot for a job that already waits on
  a booting ephemeral worker, and GitHub gives the job to whichever runner comes
  online first, usually the slot's. Why: a job sent to `ephemeral` while the pool was
  full waited about a minute for its Machine, several after an image change, even
  when a slot freed seconds later. The controller looks for such jobs on every
  GitHub message and as soon as a pool host becomes ready, starts at most one runner
  per waiting job, never more than the free slots of ready hosts and `MAX_RUNNERS`
  allow, and never creates or starts a host for it. The runner left over releases
  itself after `RUNNER_IDLE_TIMEOUT`. This is on by default for every pool placed
  before `ephemeral`; set `speculative: false` on a pool to keep plain placement.
  Status reports `speculated` per pool and `speculative` in the placement.
- `EPHEMERAL_SCALE_SET_NAME` registers a second scale set, served by the same
  controller, whose jobs always get their own ephemeral worker: a workflow opts a
  job out of the pools with `runs-on: <that name>` without taking the pools out of
  placement. Both scale sets share `MAX_RUNNERS`, the usage budget, the retirement
  journal, the launch gate and the status file. Drain the second scale set's workers
  before unsetting it: a worker of a scale set the controller no longer serves stops
  the controller, as any unknown runner registration does.
- A pool slot's workspace is a host directory of its own for every job, bind-mounted
  at `/home/runner/_work` from the host's work root (`/data/workspaces` on Fly,
  `/var/lib/runneryard/workspaces` on Hetzner and static hosts, both `0700`).
  Removing a slot removes its container, renames the directory aside and answers;
  the directory is deleted afterwards, outside Docker. Why: `docker rm` deleted the
  workspace from the container's writable layer before it returned, 12 s for a job
  that left 3.2 GB in 250,000 files and more for larger ones, which held the host
  agent's lock past the controller's 20 s call timeout and showed as "pool host did
  not answer". The same removal now answers in under half a second. On Hetzner and
  static hosts the agent is not root, so a short-lived container of the slot's image
  deletes the directories: it sees the work root only, has no network, a read-only
  root filesystem, only the `DAC_OVERRIDE` and `FOWNER` capabilities, and never
  pulls. Neither deletion follows links. `min_free_disk_gb` now also counts the work
  root, and a start low on disk waits up to 30 s for pending deletions before it
  drops the slot's caches.
- A Fly pool can follow a runner image tag such as `:current` instead of a pinned
  digest. The controller resolves the tag every 5 minutes, creates new hosts pinned
  to the digest it read, and replaces hosts on an earlier digest one at a time
  through the usual drain, so no job is interrupted; a digest whose host fails to
  create or boot stops further replacements until a host on it is ready. Trust
  boundary: to read the tag, the controller sends the pool's Fly token to
  `registry.fly.io` only, over HTTPS, with no redirect followed; every other
  registry is read anonymously, and hosts still never hold a registry credential.
  A pool with a pinned digest behaves exactly as before. Hetzner and static pools
  still require an exact image.

## 0.5.4 (2026-10-12)

- A pool host fills every slot's cache volumes from the runner image before it reports
  ready (`host boot` once the image is present, `host install` after its pull), instead
  of leaving the copy to each slot's first start. Why: Docker copies the image's files
  at a volume's path into the volume the first time a container mounts it, and an
  image that ships a pnpm store and Playwright's browsers there (3.2 GB) took 50 s to
  start its first slot and 99 s for three slots at once, past the controller's 90 s
  start timeout, so those jobs went to the next tier. A volume that already has files
  is left as it is, so a restarted host only runs one short container per slot. A
  host that cannot fill its caches logs a warning and reports ready anyway.

## 0.5.3 (2026-10-12)

- A Fly worker delete that Fly refuses with `412` because the Machine is already being
  destroyed counts as done. Why: a worker that exits after its job is destroyed by Fly
  itself (`auto_destroy`), and the controller's delete for the job completion can land
  in the same instant; the refusal reached the listener as a permanent failure and
  stopped the controller, which Fly restarted. The adapter now reads the Machine back
  and accepts `destroying`, `destroyed`, or gone; any other state stays an error.
- A pool slot with a whole `slot_cpus` is pinned to its own cores (`--cpuset-cpus`)
  when the host has a core for every slot CPU, instead of getting a CPU quota (`--cpus`).
  Why: a quota limits CPU time but leaves every host CPU visible, so `nproc`, Node's
  `os.availableParallelism()` and Go's `runtime.NumCPU()` reported eight CPUs in a
  two-CPU slot of an eight-CPU host. A test runner that sized its workers by it ran
  seven test files in two CPUs' time, and a suite that takes about 2 minutes on a
  disposable worker hit its 10-minute timeout in a slot. A fractional `slot_cpus`, or
  more slot CPUs than the host has, keeps the quota, and the host warns at boot.

## 0.5.2 (2026-10-12)

- A pool slot no longer keeps `/home/runner/_work` between jobs: every job checks
  out into an empty workspace, as on a disposable worker. Why: `actions/checkout`
  reuses a workspace it finds, and a sparse checkout left the previous job's files
  outside its pattern in place, some of them from another commit; tests that read
  the tree then failed on a production pool trial. The unused
  `runneryard-slot-<n>-work` volumes of an older host are removed with the slot's
  caches when its disk runs low.
- `runneryard doctor`'s `fleet capacity margin` counts only the apps of the worker app's
  Fly organization. Why: `fly apps list` returns the apps of every organization the
  operator belongs to, while Fly's Machine limit is per organization, so the Machines
  of another organization made a fleet that fit look over the limit. The check also
  warns, instead of passing, when an app's Machines cannot be listed, since that app
  was counted as empty.

## 0.5.1 (2026-10-12)

- A closed launch gate is probed at its `retry_at`, not at the next message from GitHub
  after it: the controller ends its long poll for GitHub's next message at that time and
  probes then. Why: with a Fly pool at `min_hosts: 0`, the first job after a quiet period
  started a stopped host in 3 to 9 s but waited up to about 50 s more for that message,
  62 s in all in the real test; it now waits for one pool tick (10 s). Backoffs after a
  capacity ceiling or a launch rejection also end on time instead of at the next
  message. Cutting a poll short loses no message: GitHub returns a message again until
  the listener deletes it, which it does only after handling it.
- Fly pools keep 1024 MB of each host's memory for the host's Docker and agent:
  `slots` times `slot_memory_mb` must now fit in `memory_mb` less that, and a pool
  without `slot_memory_mb` gives each slot an equal share of the rest instead of no
  limit. `runneryard host boot` also gives itself, and so `dockerd`, `containerd` and
  the agent, the out-of-memory score systemd gives Docker (`-500`). Why: a slot could
  use the whole Machine, and a job that exhausted memory could get the host's `dockerd`
  killed, ending every slot on the host at once. A 0.5.0 configuration with four slots
  of 4096 MB on 16384 MB is now refused; 3584 MB, as in the documented example, fits.
  Hosts created before keep their limits until they are replaced.
- A Fly pool can use a private runner image, such as one in the worker app's
  `registry.fly.io` repository. `runneryard host boot` no longer pulls the image into the
  host's Docker: it imports it from the image layer the Machine booted from, mounted
  read-only, so the host holds no registry credential and nothing Fly wrote at boot enters
  the image. It refuses a layer without `runner-entrypoint`, keeps the image's
  environment and `USER runner`, and pulls the image as before if the import fails.
  Slots run the imported image with `--pull never`. The host's Docker now keeps images on
  `overlay2` rather than the containerd image store, which took 116 s instead of 21 s to
  import the same image. Why: the host's Docker pulled the image a second time, about
  45 s of a new host's start, and could not pull a private one at all. A new host was
  ready 59 and 72 s after its creation in the real test, against 60 to 100 s before.
  Hosts created before keep their Docker storage and pull until they are replaced;
  Hetzner and static pools still need a public image.
- Jobs now receive the worker image's environment. `runner-entrypoint` starts as
  `runner`, switches to root with `sudo` to start Docker, then starts the runner as
  `runner` again; `sudo` reset the environment on the way, so the runner got only the
  lease variables and the few the entrypoint sets itself. Every `ENV` of a derived image
  (`npm_config_store_dir`, `PLAYWRIGHT_BROWSERS_PATH`, an extended `PATH`) was dropped,
  on ephemeral workers and in pool slots alike, although the derived image guide
  recommends them. The entrypoint now carries its whole starting environment across
  `sudo` for the runner; the root steps keep `sudo`'s. Why: a warm package store or
  browser directory set with `ENV` was never used, and each job downloaded it again.
  Check a derived image before upgrading: what its `ENV` sets now applies to every job,
  so a store path must be writable by `runner`, as the guide already requires. Jobs
  also see what the provider passed to the worker, such as Fly's `FLY_*` variables;
  `runner` already had passwordless `sudo`, so nothing new becomes readable.

## 0.5.0 (2026-10-12)

- Preview: `runneryard merge-queue tick` runs one tick of a merge queue inside a GitHub
  Actions workflow of the repository it serves, with its own GitHub App; it never runs in
  the controller or with its credentials. Pull requests labeled `queue` by someone with
  write access are stacked on the target, tested by their own CI on the top of the stack,
  and land by a fast-forward of the target, so GitHub marks them merged. A failure is
  re-run once, then the stack is split until the culprit is rejected. State lives in one
  status comment per pull request whose markers are only trusted from the App's bot.
  Without the App, the example workflow runs a read-only dry run that prints the plan.
  See `docs/merge-queue.md` and `docs/examples/merge-queue.yml`. Why: GitHub's native
  merge queue needs a ruleset, which GitHub Free does not offer on private repositories,
  and a hosted queue needs access to the code.
- Preview: `runneryard merge-queue setup --github <repository URL>` creates the queue's
  own GitHub App from a manifest, private to the owner and without a webhook, waits for
  its installation on the repository, verifies its permissions, and stores
  `MERGE_QUEUE_APP_CLIENT_ID` and the sealed secret `MERGE_QUEUE_APP_PRIVATE_KEY` there
  through gh. The key is never printed or written to disk. `--client-id` and
  `--private-key-file` add a further repository of the same owner to an existing queue App.
- Fix: `runneryard auth github create` prefilled an App name such as
  `RunnerYard <owner> <repository> <suffix>` that could exceed GitHub's 34 character limit,
  so the operator had to shorten it by hand. Names now fit: the repository is truncated or
  dropped first, then the owner, and the random suffix stays whole.
- Host pools (preview): `RUNNER_POOLS_FILE` declares machines that stay up and run
  several jobs at once, one per slot, with the slot's Docker images, build cache,
  workspace and tool caches kept between jobs. A pool is either Hetzner servers the
  controller creates and returns before their next paid hour (`min_hosts`,
  `max_hosts`), or machines the operator prepared with `runneryard host install`.
  The controller tries the tiers in `placement` order and moves to the next one only
  when a pool is full, so the per-job ephemeral provider stays the overflow; without a
  pools file nothing changes. Pools require `trust: private`: the controller checks
  with GitHub at startup and every hour that the target is a private or internal
  repository, or a runner group that refuses public repositories, and stops
  otherwise, because slots on one host are privileged containers that a hostile job
  could reach. The controller drives hosts over SSH with its own ed25519 key, forced to
  the host agent on every host, and pins every host key. `status.json` gains a
  `compute` object with each pool's hosts, busy slots, overflow count and billed
  hours. Why: 350 three-minute jobs a day cost about EUR 600 a month on per-job
  Hetzner servers billed by the started hour; one warm four-slot host costs about EUR
  81, and a free slot starts a job on warm caches in seconds. See `docs/pools.md`.
- Fly pools (preview): `source: fly` runs pool hosts as Fly Machines in an app of their
  own (`fly_app`), each with a volume for Docker's data. An idle host is stopped after
  `idle_stop_after` instead of deleted, so its volume keeps the image and the caches and
  it bills only for storage; the next demand starts it again before creating a new
  host. `min_hosts` stay running, `max_hosts` counts running and stopped hosts, and
  stopped hosts beyond `keep_stopped` are deleted after `delete_stopped_after`. The
  Machine boots the runner image with the new `runneryard host boot`, which starts
  Docker on the volume and serves the host agent over SSH on the Machine's private
  address, with no `sshd`, and refuses to start without that address; the controller
  must run in the same Fly organization. The pool never touches a Machine without its
  three pool labels. Status gains the
  `stopping`, `stopped` and `starting` phases and `host_stops` and `host_starts`. Pool
  images stay public: no registry credential is placed on a host. Why: Fly bills per
  second, so a stopped host with warm caches costs a few dollars a month, and one
  Machine running four jobs leaves more of the organization's Machine limit to the
  rest of the fleet.
- A full pool that is adding a host asks the controller to probe again after one pool
  tick instead of after the capacity backoff: `provider.CapacityError` gains
  `RetryAfter`, bounded to 5 s at least, and a hinted refusal leaves the backoff as it
  was. Why: in the first real Fly test the backoff (one minute, then two) outlasted
  the new host's boot, so queued jobs waited on the busy host while the new one sat
  empty.
- `runneryard budget estimate --provider hetzner` prices the Hetzner bill: every job gets
  its own server, and Hetzner rounds each server up to the next hour, so each job is billed
  whole hours whatever its length. The output shows the hours billed per job, the share
  actually used, and warns when most of the bill is unused time. Built-in Germany and
  Finland list prices (in force since 2026-06-15, read 2026-10-11) cover `cx33`, `cx53`,
  `cpx22`, `cpx32`, `cpx42` and `ccx33`; `--hourly-price` covers other types and
  locations. A shape flag of the other provider (`--cpus` with Hetzner, `--server-type`
  with Fly) is refused instead of silently ignored. Why: since the 2026-06-15 price
  change, 350 three-minute jobs a day cost about EUR 600 a month on per-job `cpx32`
  servers, against about USD 105 on Fly.
- The Hetzner guide documents hourly billing and why the default server type stays
  `cpx32`: `cx33` is about a quarter of the price for the same 4 vCPU and 8 GB, but
  Hetzner lists its cost-optimized line as currently not available, and a type that
  cannot be ordered rejects every launch.

## 0.4.10 (2026-10-11)

- `runneryard budget estimate` sizes `RUNNER_USAGE_BUDGET` and the Fly compute bill from
  a workload, offline: jobs a day, minutes per job, and the worker shape. Each job is
  charged its duration plus the machine's boot, registration and teardown (`--overhead`,
  75 s by default), and the suggestion adds one `RUNNER_MAX_LIFETIME` per `MAX_RUNNERS`,
  since every launch holds that reservation until it settles; a budget sized to usage
  alone stops admitting workers before the usage is reached. The suggestion is printed
  as a duration the controller accepts. Prices are Fly's iad/ewr rates of 2026-10-11;
  `--price-multiplier` applies another region's markup (cdg: 1.1346). Why: one fleet ran
  8.7 days from exhaustion on 2026-09-05, and a workload measured on 2026-10-08 needed
  34 times the scaffold's 166h40m.
- The Go toolchain and the release image move to Go 1.26.9: govulncheck reports ten
  standard library vulnerabilities in 1.25.14 (`net/http`, `net/textproto`,
  `crypto/tls`, `os`), and Go 1.25 no longer receives security fixes. The worker image
  moves to the current `ubuntu:24.04` digest; the pinned Docker, BuildKit, Compose and
  fuse-overlayfs packages are unchanged.

## 0.4.9 (2026-09-06)

- `lint-workflows`: a `pull_request` trigger whose `types` list names neither `opened`
  nor `synchronize` (a `labeled`-only release gate, a `closed`-only cleanup) never fires
  on the push every pull request makes; it is no longer reported as
  `unfiltered-pull-request`. The remedy now names that shape: restrict `types:` to the
  events the workflow judges.

## 0.4.8 (2026-09-06)

- `runneryard init --with-workflows` writes the shape that costs least on a fleet:
  `.github/workflows/ci.yml` (one job plans from the merge-base and runs the guards as
  steps, a lane job per scope, one `gate` job to require) and `.github/ci-scopes.json`.
  `lint-workflows` no longer reports a workflow that plans from the merge-base as
  unfiltered; on the recipe its only finding is the gate.
- `doctor` reads the fleet as it runs: `budget horizon` (fails under 14 days of remaining
  budget at the trailing burn rate, or on a degraded fleet; read over `fly ssh console`)
  and `fleet capacity margin` (`--fly-machine-limit <n>`: MAX_RUNNERS against the room the
  organization's Machine limit leaves once every other app's Machines, stopped ones
  included, are counted). Both were missing on 2026-09-05, when doctor was green 8.7 days
  before the budget ran out and two days after MAX_RUNNERS had crossed the limit.

## 0.4.7 (2026-09-06)

- `runneryard lint-workflows`: an offline lint of `.github/workflows` that says what a
  repository's shape will cost on an ephemeral fleet, with the remedy per finding:
  duplicated planner callers, tiny jobs (a machine for seconds of work), pull_request
  triggers without a path filter, jobs without a budget, unparseable files. Advisory by
  default, `--strict` for CI, `--json` for tooling. Motivation, measured 2026-09-05 on a
  monorepo: 3 753 fleet jobs in a day at a median of six seconds, 90 hours of job time for
  292 machine-hours billed, 29 machines for a one-file pull request (20 of them under
  thirty seconds of work; the figure of 66 in earlier drafts counted check-runs, 37 of
  which were skipped).
- `doctor` fails when the controller has no `ALERT_WEBHOOK_URL` (check "controller
  alerting"). Every degraded state - usage budget exhausted, provider capacity ceiling,
  a retirement stuck - leaves jobs queued behind a healthy-looking controller, and the
  webhook is the only channel that says so; on 2026-09-05 a fleet ran with no webhook
  while its budget stood 8.7 days from exhaustion, and doctor was green.

## 0.4.6 (2026-09-06)

- The worker entrypoint gives `/home/runner` back to `runner` when a derived
  image left root-owned paths there, and records the first such path in
  `/run/runneryard/home-ownership-restored`; the generated canary fails on that
  marker and probes the home itself. Why: the first derived image of a fleet,
  built as the guide said ("install as root, end as runner"), ran `npm`, `pnpm`
  and Playwright with `HOME=/home/runner` and left `~/.npm` and `~/.cache`
  owned by root; on 2026-09-05 every job that ran `npm install` on it died with
  "cache folder contains root-owned files" (EACCES) - three pull requests red
  within the same hour, none at fault. The guide now carries the rule and its
  example drops the caches and chowns the home before `USER runner`.

## 0.4.5 (2026-09-05)

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
