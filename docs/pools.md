# Host pools

A pool keeps machines running and starts each job in a free slot on one of
them, instead of creating a machine per job. A slot keeps its Docker images,
build cache and tool caches between jobs, so a job starts on warm data, and a
host that is already paid for runs many jobs in a row.

Pools are a preview: covered by tests, not yet proven on a production fleet.
Start with one host on a test repository, keep `ephemeral` as the last tier,
and expect the pools file to change before 1.0.

Pools are optional. Without `RUNNER_POOLS_FILE` the controller behaves exactly
as before: one disposable worker per job on the configured `COMPUTE_PROVIDER`.
With it, the controller tries its tiers in a fixed order and moves to the next
one only when a tier is full, so a job never waits for a pool host while
another tier can start it now.

## When a pool fits

- **Every repository the fleet serves is private and trusts the others.** Slots
  on one host are privileged containers: a job can reach the host, and through
  it the other slots and the next job's caches. See [Trust](#trust).
- **Jobs are short and frequent.** Hetzner bills each server by the started
  hour; a per-job server pays a full hour for a three-minute job. A pool host
  runs dozens of such jobs in the hour it is billed.
- **Start latency matters.** A free slot starts a job in seconds, with the
  previous job's images and caches still in place.
- **A Fly organization runs out of Machines.** One Fly pool host runs as many
  jobs as it has slots, so a fleet that hits the organization's Machine limit
  with one Machine per job fits several times more jobs under it.

Keep the ephemeral tier for everything else: public repositories, untrusted
pull requests, or a fleet that needs every job on a fresh machine.

## How placement works

Each tier is a pool or `ephemeral` (the `COMPUTE_PROVIDER` workers). A launch
goes to the first tier in `placement` with room. A pool with no free slot
answers "full" at once, without waiting for a host to boot, and the launch
moves on to the next tier; any other failure is reported as it is. A full
last tier makes the controller back off as on any provider capacity refusal,
so keep `ephemeral` last unless the fleet must never leave its pools.

A full pool also asks for one more host (within `max_hosts`), which serves the
next jobs once it is ready. While that host is on its way, the pool's refusal
asks the controller to try again after one pool tick (10 s) rather than after
the capacity backoff of one minute and more, so the queued jobs reach the new
host as soon as it is ready; a pool full at `max_hosts` gets the usual
backoff. The controller wakes itself for that probe; it does not wait for
GitHub's next message. Slots are packed: a job goes to the busiest host
that still has room, so the least busy hosts empty first and can be returned.

`MAX_RUNNERS` still caps the whole fleet, pools and ephemeral workers
together, and `RUNNER_USAGE_BUDGET` still reserves `RUNNER_MAX_LIFETIME` per
launch. A pool with more slots than `MAX_RUNNERS` leaves the rest idle.

A tier present in the file but missing from `placement` drains: the controller
starts nothing there, keeps tracking the workers it still runs, and destroys
them as usual.

## When hosts are returned

| Source | A host without jobs is returned |
| --- | --- |
| `hetzner` (billed per started hour) | Shortly before its next paid hour starts, with two minutes of margin. |
| `fly` (billed per second) | Stopped after `idle_stop_after`, not deleted: its volume keeps the caches, and the next demand starts it again before creating a new host. See [Fly pools](#fly-pools). |
| `static` | Never: a static pool never deletes a machine, whatever its `billing`. |

`min_hosts` hosts (the oldest ready ones) are kept warm whatever their billing.
A host the controller created and that never answers within ten minutes is
deleted. A host that stays silent for longer than `RUNNER_MAX_LIFETIME` plus
two minutes is deleted too: every slot container has a deadline within that
lifetime, so nothing on it can still be a job. Before deleting a host for
being idle, the controller lists its slots one last time and keeps it if a job
appeared.

## Trust

Slots share a kernel and run privileged, so a hostile job can read or tamper
with whatever the other slots on its host run, and with the caches the next
job on its slot reuses. RunnerYard therefore requires `trust: private` in the
pools file and checks the GitHub target with the App's (or the token's)
credentials:

- at startup, before any pool host is contacted, and
- every hour afterwards.

A repository target must be private or internal. An organization target's
runner group (`RUNNER_GROUP`, default `Default`) must have
`allows_public_repositories` set to false. A target that answers "public"
stops the controller; a failed check at startup refuses to start, and a failed
hourly check is logged and retried.

Internal repositories are visible to every member of the enterprise. Treat
them as one trust domain with the repositories they share a pool with, and
disable workflows from fork pull requests on every repository the fleet
serves: a fork's workflow runs the fork author's code.

What a compromised job can reach on a pool host: the other slots and their
JIT configurations, the slot caches, the host's own SSH host key, and the
network the host is attached to. What it cannot reach: the controller's SSH
key, `HCLOUD_TOKEN`, the Fly API token, the GitHub App key, or, on Hetzner and
static hosts, another host's SSH host key. Fly hosts have a wider surface:
see [Fly pools](#fly-pools) and the security notes it links.
Nothing on a host lets it log in to the controller; the controller connects to
the hosts, never the reverse.

## Controller key and host keys

The controller reaches its hosts over SSH with one ed25519 key, generated once
and kept on the controller:

```sh
ssh-keygen -t ed25519 -N '' -C runneryard-pool -f /var/lib/runneryard/pool_ed25519
chmod 600 /var/lib/runneryard/pool_ed25519
```

Each host installs the public half as a forced command:
`restrict,command="/usr/local/bin/runneryard host"`. The key can only run the
host agent, which answers four requests (`info`, `list`, `start N`,
`remove N LEASE`); it cannot open a shell, forward ports, or run anything
else, and a slot only accepts the exact runner image the host was installed
with.

The controller pins every host's SSH host key. For Hetzner and Fly hosts it
derives a distinct key per machine from its own key (HMAC-SHA256 over the
machine name) and installs it at creation, through cloud-init or a Machine
file, so no first-connection trust is needed. For static hosts, the operator
lists each host's public key.

## Configuration

`RUNNER_POOLS_FILE` points at a YAML file. Unknown keys are refused.

| Variable | Purpose | Default |
| --- | --- | --- |
| `RUNNER_POOLS_FILE` | The pools file. Unset, there are no pools. | Unset |
| `RUNNER_POOL_SSH_KEY_FILE` | Controller SSH key; overrides `ssh_key_file`. | Unset |
| `COMPUTE_PROVIDER` | The ephemeral tier's provider, or `none` for pools only. `none` requires a pools file. | `fly` |

```yaml
trust: private
ssh_key_file: /var/lib/runneryard/pool_ed25519
placement: [warm, ephemeral]   # default: every pool in file order, then ephemeral
pools:
  - name: warm
    source: hetzner
    server_type: cpx42          # 8 vCPU, 16 GB
    location: fsn1              # default: RUNNER_HETZNER_LOCATION
    firewall_id: 1234567        # its own firewall, see below
    slots: 4
    slot_cpus: 2
    slot_memory_mb: 3584
    min_hosts: 1
    max_hosts: 3
  - name: office
    source: static
    billing: owned
    hosts:
      - name: box-1
        address: 10.0.0.5        # port 22 unless given
        host_key: "ssh-ed25519 AAAA..."
```

| Key | Applies to | Meaning | Default |
| --- | --- | --- | --- |
| `name` | all | Lowercase letters, digits and hyphens; not `ephemeral`. | Required |
| `source` | all | `fly`, `hetzner` or `static`. | Required |
| `runner_image` | all | Exact image every slot runs. Pin a digest. | `RUNNER_IMAGE` |
| `slot_cpus`, `slot_memory_mb` | all | Docker limits per slot; `0` for none. A Fly pool always limits memory, see `memory_mb`. | `0`; Fly `slot_memory_mb`: an equal share of `memory_mb` less 1024 MB |
| `caches` | all | Runner paths kept between a slot's jobs. | `~/.cache`, `~/.npm`, `~/go` |
| `min_free_disk_gb` | all | Free disk a start requires; below it the slot's caches are dropped first. | `20` |
| `hourly_price` | all | Host price for reporting. | Hetzner and Fly: the list price of known types and regions (required otherwise). Static: `0` |
| `slots` | hetzner, fly | Slots per host, 1 to 64. | Required |
| `min_hosts`, `max_hosts` | hetzner, fly | Hosts kept running, and the ceiling (at most 100). On Fly, stopped hosts count toward `max_hosts`. | `0`, required |
| `server_type`, `location`, `server_image` | hetzner | Server shape and image. | `location` from `RUNNER_HETZNER_LOCATION`, image `docker-ce` |
| `firewall_id` | hetzner | Firewall attached to every pool host. Must differ from `RUNNER_HETZNER_FIREWALL_ID`. | Required |
| `network_id` | hetzner | Private network to attach. Hetzner firewalls do not filter it. | Unset |
| `billing`, `currency` | static | `owned`, `per_second` or `per_started_hour`, for reporting only. | `owned` |
| `hosts` | static | Prepared machines; each sets its slots in its own install. | Required |
| `fly_app` | fly | The pool's own Fly app. Must differ from `RUNNER_FLY_APP` and the controller's app. | Required |
| `region` | fly | Region of every host and volume. | `RUNNER_FLY_REGION` |
| `cpu_kind`, `cpus`, `memory_mb` | fly | Machine shape. `slots` times `slot_memory_mb` must fit in `memory_mb` less 1024 MB, kept for the host's Docker and agent; each slot gets at least 512 MB. | `performance`, required, required |
| `volume_gb` | fly | Size of each host's Docker volume, 10 to 500. | `50` |
| `idle_stop_after` | fly | An empty host beyond `min_hosts` is stopped after this. | `5m` |
| `keep_stopped` | fly | Stopped hosts kept for the next demand, at most `max_hosts`. | `1` |
| `delete_stopped_after` | fly | Stopped hosts beyond `keep_stopped` are deleted after this. | `6h` |

Every slot also keeps its own Docker data root, so `docker build` reuses its
layers. The previous job's containers, networks and volumes are removed before
the next job; its images and build cache stay.

The workspace, `/home/runner/_work`, is never kept: every job checks out into
an empty one, as on a disposable worker, and it cannot be listed in `caches`.
RunnerYard 0.5.1 and earlier kept it, and a sparse checkout then found the
previous job's files outside its pattern, some of them from another commit
(`actions/checkout` does not remove them). A host started by an older agent
keeps an unused `runneryard-slot-<n>-work` volume per slot until the disk runs
low, when it is removed with the slot's caches.

## Hetzner pools

Pool hosts are Cloud Servers in the same project as `HCLOUD_TOKEN`, created
with the labels `runneryard-pool-host=true`, `runneryard-controller` and
`runneryard-pool`, and named `ry-<pool>-<8 hex>`. The ephemeral Hetzner
adapter ignores them, and the pool never touches a server without all three
labels.

Create a firewall for the pool that admits TCP 22 from the controller's
address only, and nothing else inbound. Pool hosts must not use the worker
firewall: it admits no SSH at all.

On first boot, cloud-init installs the derived host key, turns off password
and root login, pulls the runner image, copies `runneryard` out of it, and
runs `runneryard host install`. The image is pulled without credentials, so it
must be public, such as the release image on GHCR. An image in a private
registry (a Fly app registry, for instance) cannot be used by Hetzner pool
hosts yet.

A host takes about two minutes to become ready. With `min_hosts: 0`, the first
job of a quiet period goes to the next tier while the host boots; keep one
host warm when that matters.

Estimate before routing: at the list price read on 2026-10-11, one `cpx42`
host (EUR 0.1114 an hour, excluding VAT and the primary IPv4) kept warm all
month costs about EUR 81, and runs four jobs at a time. 350 three-minute jobs
a day on per-job `cpx32` servers cost about EUR 600 a month.

## Fly pools

A Fly pool host is one Fly Machine in the pool's own app, `fly_app`, with one
volume mounted at `/data` for Docker's data. The Machine boots the runner
image itself, as root, with `runneryard host boot`:

1. reads the host configuration, its derived host key and the controller's
   public key, written as Machine files at creation;
2. starts `dockerd` with its data root on the volume, and removes any slot
   container a previous run left behind. `dockerd`, `containerd` and the
   agent run with the out-of-memory score systemd gives Docker (`-500`), and
   every slot has a memory limit that leaves 1024 MB to them: a job that
   exhausts memory is killed in its own slot instead of taking the host's
   Docker, and every slot with it, down;
3. serves the host agent over SSH on the Machine's private address, port
   2222 (Fly's own SSH server, used by `fly ssh console`, holds port 22); it
   refuses to start when `FLY_PRIVATE_IP` is missing or is not an IP address;
4. imports the runner image into the host's Docker from the Machine's own
   root filesystem when the volume does not have it yet, and pulls it
   instead if the import fails (see [Runner image](#runner-image)).

Every step is safe to repeat, because a stopped Machine runs them again on its
next start: the root filesystem is reset, the volume is not. There is no
`sshd` and no login: the agent's own SSH server admits only the controller's
key, as user `runneryard`, and answers only the four agent requests.

Machines carry the metadata `runneryard-pool-host=true`,
`runneryard-controller` and `runneryard-pool`, plus `runneryard-pool-image`
for the image they boot. The pool never lists, stops, starts or deletes a
Machine without all three labels, nor any worker Machine. Volumes are named
`ry_<8 hex>_<8 hex>`, the first part derived from the controller ID and the
pool name; an unattached volume with that prefix and older than 30 minutes is
left over from a failed creation or deletion, and is deleted.

### Setup

The hosts listen on Fly's private network only, so the controller must run as
a Fly Machine in the same organization. Put the controller and the pool on a
[custom private network](https://fly.io/docs/networking/custom-private-networks/)
of their own: a job that escapes its slot then reaches the controller and the
other pool hosts, not every app of the organization. An app's network is set
when the app is created.

```sh
fly apps create ci-controller --org my-org --network ci-pool
fly apps create ci-pool --org my-org --network ci-pool
fly tokens create deploy --app ci-pool --expiry 8760h   # RUNNER_POOL_FLY_API_TOKEN
```

The controller reads the pool app's token from `RUNNER_POOL_FLY_API_TOKEN`,
or from `FLY_API_TOKEN` when the pool and the ephemeral workers share one
token. Use a deploy token scoped to the pool app, never an organization
token. Keep the pool app free of secrets: hosts ignore app secrets, and
nothing else should run in that app.

```yaml
trust: private
ssh_key_file: /data/pool_ed25519
placement: [burst, ephemeral]
pools:
  - name: burst
    source: fly
    fly_app: ci-pool
    region: cdg                 # default: RUNNER_FLY_REGION
    cpu_kind: performance
    cpus: 8
    memory_mb: 16384
    volume_gb: 50
    slots: 4
    slot_cpus: 2
    slot_memory_mb: 3584        # 4 x 3584 leaves 2 GB to the host
    min_hosts: 0                # running hosts kept whatever the load
    max_hosts: 3                # running and stopped hosts together
    keep_stopped: 1             # stopped hosts kept for the next burst
    idle_stop_after: 5m
    delete_stopped_after: 6h
```

### Stop, start and delete

- **Demand.** A full pool asks for one more host. It starts the most recently
  stopped host first, whose volume still has the image and the caches, and
  creates a new host only when no stopped host can be started.
- **Idle.** An empty host beyond `min_hosts` is stopped after
  `idle_stop_after`. A stopped Machine bills only for its volume and its root
  filesystem.
- **Reserve.** The `keep_stopped` most recently stopped hosts wait for the next
  burst. The others are deleted with their volume once stopped for
  `delete_stopped_after`.
- **Failures.** A host that fails to start twice, or that Fly cannot start for
  lack of capacity, is deleted, and the next demand creates a new one. A
  stopped host from another `runner_image` is deleted, never started.
- **Outside changes.** A host stopped or started by hand (`fly machine stop`)
  is followed at the next listing.

So `min_hosts: 1` keeps one host running at all times, and `min_hosts: 0` with
`keep_stopped: 1` keeps one stopped host ready instead: a few dollars a month,
at the price of a start delay on the first job of a burst.

### Cost

Fly bills a running Machine per second. At the rates read on 2026-10-11, a
`performance` Machine with 8 vCPUs and 16 GB costs about USD 0.367 an hour in
`iad` and USD 0.416 an hour in `cdg`, so about USD 300 a month when kept
running all month in `cdg`. A stopped host costs its volume (USD 0.15 per GB
and month, so USD 7.50 for 50 GB) and its root filesystem. Set `hourly_price`
for a region without a listed multiplier.

### Start times

Measured in real tests (`cdg`, `performance` 8 vCPUs, 16 GB, a 900 MB
runner image):

- A new host is ready 59 to 72 s after its creation: Fly pulls the image for
  the root filesystem (about 30 s), then host boot imports it from there into
  the host's Docker (23 to 28 s). A burst shorter than that is over before the
  new host can take a job.
- A stopped host is ready 3 to 9 s after its start, with its images, build
  cache and slot caches as it left them.
- With `min_hosts: 0`, the first job after a quiet period starts a stopped
  host and is placed on it at the controller's next probe, 10 s later. In the
  test it started 62 s after it was queued, because the probe then waited for
  the controller's next GitHub message (up to about 50 s); the controller now
  wakes itself for the probe. `min_hosts: 1` takes it at once, for the price
  of a host running all month.

### Organization Machine limit

A Fly organization has a Machine limit. Ephemeral workers use one Machine per
job; a pool host uses one Machine for `slots` jobs. Stopped hosts are still
Machines, so keep `max_hosts` within what the limit leaves to the other apps.

### Runner image

The Machine boots the runner image through Fly, which pulls it with the
organization's own access: the image may be private, such as the worker
app's repository `registry.fly.io/<worker-app>`, and no host holds a registry
credential. Host boot then imports that same filesystem into the host's
Docker for the slots, from a read-only mount of the image layer under the
Machine's root, with the image's environment and `USER runner`. It does not
read the files Fly writes at boot, the host key among them; see
[Private runner images](security.md#private-runner-images).

- The imported image has the configured name, or `<name>:sha256-<hex>` for an
  image pinned by digest, since Docker cannot tag a digest. Slots start with
  `--pull never` from that name.
- The import keeps no entrypoint, command, working directory or labels of the
  image (slots set their own entrypoint), and sets `USER runner`: a derived
  image must end with `USER runner`, as the
  [derived image guide](derived-images.md#build-rules) requires.
- The host's Docker keeps images on `overlay2`, not on the containerd image
  store that Docker 29 uses by default: the same import took 21 s on the
  first and 116 s on the second.
- If the import fails, the host logs why and pulls the image instead, which
  works only for a public image. A private image then leaves the host not
  ready, with both errors in its log (`fly logs --machine <id>`).

## Static hosts

Prepare each machine once, as root, with the runner image the pool uses:

```sh
runneryard host install --image ghcr.io/gwendall/runneryard@sha256:... \
  --slots 4 --slot-cpus 2 --slot-memory-mb 3584 \
  --authorized-key "$(cat /var/lib/runneryard/pool_ed25519.pub)" \
  --block-metadata
```

`install` creates the `runneryard` account with the forced-command key, writes
`/etc/runneryard/host.json`, and pulls the image. `--harden-sshd` also turns
off passwords, root login and forwarding for the whole machine; use it only on
a machine dedicated to the pool. `--block-metadata` keeps containers away from
`169.254.169.254` on clouds that serve credentials there.

The `runneryard` account is in the `docker` group, which is root-equivalent on
the machine. Only the forced command can use it.

Read the host key to list in the pools file with
`cat /etc/ssh/ssh_host_ed25519_key.pub` on the machine itself; a key read over
the network with `ssh-keyscan` is only as trustworthy as that network.

Changing the image or the slots means running `install` again; a host whose
image differs from the pool's `runner_image` stops taking jobs until it is.
Hetzner pools handle this themselves: after a `runner_image` change, hosts on
the old image take no new job and are replaced once empty.

## Status

`status.json` gains a `compute` object when pools are configured, refreshed
every five seconds (abridged):

```json
"compute": {
  "tiers": ["warm", "ephemeral"],
  "pools": [{
    "name": "warm", "billing": "per_started_hour", "hourly_price": 0.1114,
    "slots_total": 4, "slots_busy": 1, "launched": 212, "overflowed": 3,
    "returned_hosts": 2, "billed_hours": 31, "cost": 3.45,
    "hosts": [{"id": "hetzner-123", "phase": "ready", "slots": 4, "busy": 1, "reachable": true}]
  }]
}
```

A host's `phase` is `creating`, `booting`, `ready`, `draining` or `deleting`,
and for Fly pools also `stopping`, `stopped` (with `stopped_at`) or
`starting`. Fly pools add `host_stops` and `host_starts`. A stopped host is
neither reachable nor billed.

`overflowed` counts launches the pool sent to the next tier. A steady rise
means the pool is too small for the load: raise `slots` or `max_hosts`.
`billed_hours` and `cost` cover what this controller process has seen since
it started.

## Removing a pool

1. Take the pool out of `placement` and set `min_hosts: 0`. It drains: no new
   jobs, running ones finish, and idle Hetzner hosts are returned at the end
   of their paid hour.
2. When `status.json` shows the pool without hosts, delete it from the file.

A Hetzner pool deleted from the file while it still has hosts leaves them
running and billed. Delete them in the Hetzner console, or with
`hcloud server list -l runneryard-pool=<name>` then `hcloud server delete`.

A Fly pool drained the same way stops its hosts, keeps `keep_stopped` of them
and deletes the others after `delete_stopped_after`; set `keep_stopped: 0`
first to have them all deleted. A Fly pool removed from the file leaves its
Machines and volumes in the pool app: `fly apps destroy <fly_app>` deletes
them all.
