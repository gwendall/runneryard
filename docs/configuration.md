# Configuration reference

`runneryard init` writes a reviewable starting point. Keep policy in the
generated TOML or environment file, credentials in the controller secret
store, and durable state on the controller volume. Never define a policy value
in both Fly secrets and TOML: a secret wins while hiding the effective value
from code review.

## Identity and GitHub

| Variable | Purpose | Starting value |
| --- | --- | --- |
| `GITHUB_CONFIG_URL` | Exact repository or organization served by the scale set. | Generated from `--github` or the current Git remote. |
| `SCALE_SET_NAME` | GitHub scale-set name and `runs-on` label. | `runneryard-linux-x64` |
| `RUNNER_GROUP` | GitHub runner group for organization fleets. | GitHub default group. |
| `CONTROLLER_ID` | Stable owner recorded in provider metadata. Changing it creates a different ownership boundary. | Same as `SCALE_SET_NAME`. |
| `GITHUB_APP_CLIENT_ID` | Dedicated or bring-your-own App client ID. | Stored by `auth github`. |
| `GITHUB_APP_INSTALLATION_ID` | Verified installation serving the selected target. | Stored by `auth github`. |
| `GITHUB_APP_PRIVATE_KEY` or `GITHUB_APP_PRIVATE_KEY_FILE` | App signing key. Use the provider secret sink or a mode-`0600` file. | Stored by `auth github`. |

`GITHUB_TOKEN` exists only as a private-canary compatibility path. Do not set it
alongside App credentials. The GitHub App flow is the production default.

## Capacity and cost

| Variable | Purpose | Generated value |
| --- | --- | --- |
| `MIN_RUNNERS` | Warm idle floor. | `0` |
| `MAX_RUNNERS` | Hard concurrent worker ceiling. | `4`, or `--max-runners` |
| `RUNNER_MAX_LIFETIME` | Forced worker deadline and per-launch budget reservation. | `2h` |
| `RUNNER_USAGE_BUDGET` | Maximum settled plus reserved worker time in the rolling window. | `166h40m` (10,000 minutes) |
| `RUNNER_BUDGET_WINDOW` | Rolling usage window. | `720h` (30 days) |
| `RUNNER_BUDGET_FILE` | Fail-closed ledger on durable storage. | `/var/lib/runneryard/budget.json` |
| `RUNNER_STATUS_FILE` | Private atomic health receipt; must differ from the ledger. | `/var/lib/runneryard/status.json` |

Start with the generated ceiling. Raise `MAX_RUNNERS` only from observed peak
job concurrency and provider quota, never from pull-request count. The operator
owns this value; repository code and pull requests must not change a live fleet
limit.

Each launch first reserves one full `RUNNER_MAX_LIFETIME`. A budget therefore
admits at most `floor(remaining budget / maximum lifetime)` new workers, even
when `MAX_RUNNERS` is higher. Set the rolling budget from an explicit monthly
worker-time allowance, and keep a separate provider spending alert for the
controller, storage, images, network, taxes, and price changes.

Lower the maximum lifetime to the longest legitimate job plus cleanup margin.
Do not use it to mask a slow test: move advisory or periodic work out of the PR
gate first. Keep `MIN_RUNNERS=0` unless measured queue latency justifies paying
for warm workers.

## Fly worker shape

| Variable | Purpose | Generated value |
| --- | --- | --- |
| `RUNNER_FLY_APP` | Dedicated secret-free worker app. | Generated from the GitHub owner. |
| `RUNNER_FLY_REGION` | Worker region. | `cdg`, or `--region` |
| `RUNNER_IMAGE` | Immutable RunnerYard controller and worker image. | Current CLI release tag. |
| `RUNNER_CPU_KIND` | Fly CPU class. | `shared` |
| `RUNNER_CPUS` | CPUs per worker. | `4` |
| `RUNNER_MEMORY_MB` | Memory per worker. | `8192` |
| `RUNNER_ROOTFS_GB` | Ephemeral root filesystem per worker. | `30` |

`FLY_API_TOKEN` is a deploy token scoped only to the worker app and belongs only
on the controller. `FLY_APP_NAME` identifies the separate controller app.

## Hetzner worker shape

| Variable | Purpose | Generated value |
| --- | --- | --- |
| `RUNNER_HETZNER_LOCATION` | Worker location. | `fsn1`, or `--region` |
| `RUNNER_HETZNER_SERVER_TYPE` | Hetzner server shape. | `cpx32` |
| `RUNNER_HETZNER_IMAGE` | Host image or prequalified snapshot ID. | `docker-ce` |
| `RUNNER_HETZNER_FIREWALL_ID` | Mandatory firewall with no inbound rules. | Operator supplied. |
| `RUNNER_HETZNER_NETWORK_ID` | Optional dedicated private network. | Empty. |

`HCLOUD_TOKEN` must be read/write scoped to a dedicated CI project and stored
only on the controller. A Hetzner firewall does not filter private-network
traffic, so leave the network ID empty unless the entire network is isolated
from production.

## Safe change procedure

1. Disable routing or choose a maintenance window; do not start two controllers
   for one scale set.
2. Change one reviewed policy at a time. On Fly, update the pinned deployment
   image. On Hetzner, set the same release tag in both `RUNNER_IMAGE` inside
   `controller.env` (workers) and `image` inside Compose (controller).
3. Set `RUNNERYARD_EXPECTED_VERSION` in the generated canary to that release.
4. Preserve the durable volume, budget ledger, retirement journal, and stable
   `CONTROLLER_ID`.
5. Replace the controller, run `doctor`, and trigger the manual canary.
6. Verify the controller version from `status`; the canary asserts the exact
   worker release. Require a green job, a complete lifecycle receipt, and zero
   remaining workers before restoring broader routing.

See [operations](operations.md) for upgrade and incident procedures, and the
[security model](security.md) before changing any trust boundary.
