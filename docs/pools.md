# Host pools

A pool keeps machines running and starts each job in a free slot on one of
them, instead of creating a machine per job. A slot keeps its Docker images,
build cache, workspace and tool caches between jobs, so a job starts on warm
data, and a host that is already paid for runs many jobs in a row.

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
next jobs once it is ready. Slots are packed: a job goes to the busiest host
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
key, `HCLOUD_TOKEN`, the GitHub App key, or another host's SSH host key.
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

The controller pins every host's SSH host key. For Hetzner hosts it derives a
distinct key per server from its own key (HMAC-SHA256 over the server name)
and installs it through cloud-init, so no first-connection trust is needed.
For static hosts, the operator lists each host's public key.

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
| `source` | all | `hetzner` or `static`. | Required |
| `runner_image` | all | Exact image every slot runs. Pin a digest. | `RUNNER_IMAGE` |
| `slot_cpus`, `slot_memory_mb` | all | Docker limits per slot; `0` for none. | `0` |
| `caches` | all | Runner paths kept between a slot's jobs. | `~/.cache`, `~/.npm`, `~/go` |
| `min_free_disk_gb` | all | Free disk a start requires; below it the slot's caches are dropped first. | `20` |
| `hourly_price` | all | Host price for reporting. | Hetzner: the list price of known types (required otherwise). Static: `0` |
| `slots` | hetzner | Slots per host, 1 to 64. | Required |
| `min_hosts`, `max_hosts` | hetzner | Hosts kept warm, and the ceiling (at most 100). | `0`, required |
| `server_type`, `location`, `server_image` | hetzner | Server shape and image. | `location` from `RUNNER_HETZNER_LOCATION`, image `docker-ce` |
| `firewall_id` | hetzner | Firewall attached to every pool host. Must differ from `RUNNER_HETZNER_FIREWALL_ID`. | Required |
| `network_id` | hetzner | Private network to attach. Hetzner firewalls do not filter it. | Unset |
| `billing`, `currency` | static | `owned`, `per_second` or `per_started_hour`, for reporting only. | `owned` |
| `hosts` | static | Prepared machines; each sets its slots in its own install. | Required |

Every slot also keeps `/home/runner/_work` and its own Docker data root, so
`actions/checkout` fetches only what changed and `docker build` reuses its
layers.

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
