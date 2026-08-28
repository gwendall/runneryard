# RunnerYard

Run one isolated GitHub Actions runner per job on infrastructure you control,
then destroy it. Zero idle workers by default, no Kubernetes required.

[runneryard.com](https://runneryard.com)

`runneryard` is a small Go controller built on GitHub's official
[`actions/scaleset`](https://github.com/actions/scaleset) client. Its compute
interface is provider-neutral. Fly Machines is available and Hetzner Cloud is
included as a preview adapter.
The controller ships as a standalone binary, a container image, and a
checksum-verifying npm launcher.

> [!WARNING]
> `runneryard` is pre-1.0. Use it for trusted private-repository workflows
> first. Never route untrusted public pull requests to a self-hosted runner.

## Quick start

The public release supports this setup flow. Run it from the repository that
will use the runners:

```sh
npx runneryard init --github https://github.com/acme/widgets
```

Use `--max-runners 2` for a conservative first canary, then raise the
operator-owned ceiling only from observed queue depth and provider spend.

This creates three reviewable files for the default Fly provider and never
uploads a credential:

- `.runneryard/controller.env.example`
- `.runneryard/fly.controller.toml`
- `.github/workflows/runneryard-canary.yml`

The generated Fly worker uses two sustained performance CPUs and 8 GB of RAM.
Shared CPUs remain an explicit option for short, bursty jobs; see
[Fly worker sizing](docs/configuration.md#fly-worker-shape) before changing the
shape or concurrency ceiling.

Next, create the isolated provider resources described in the
[Fly guide](docs/providers/fly.md). Create a private GitHub App owned by the
target account and send its one-time key directly to the controller secret
store:

```sh
npx runneryard auth github create \
  --github https://github.com/acme/widgets \
  --controller-app acme-ci-controller
```

The browser shows the owner and exact permission before creating or installing
anything. No GitHub token or private key is copied into the terminal, and no
credential is sent to RunnerYard. Then verify the provider boundary:

```sh
npx runneryard doctor --provider fly \
  --controller-app acme-ci-controller \
  --worker-app acme-ci-runners
```

Initialize the durable usage ledger once, deploy the pinned controller image,
and trigger the generated canary. A successful canary has three receipts:
GitHub reports a green job, the controller records the complete lifecycle, and
provider inventory returns to zero workers. Nothing routes normal CI to the
fleet until you explicitly enable the generated label.

The worker image includes the pinned Node patch in the Actions toolcache plus
Docker Buildx/BuildKit. This avoids downloading the same Node runtime on every
disposable worker. Its nested daemon uses `fuse-overlayfs` when the provider
presents a layered root filesystem and Docker's native snapshotter on a plain
VM filesystem, avoiding both invalid overlay-on-overlay mounts and Docker's
deep-copy `vfs` fallback. The generated canary verifies that toolcache through
`actions/setup-node`, checks the selected storage path, and completes a real
BuildKit build before normal CI is routed to the fleet.

For Hetzner Cloud, generate its provider-specific scaffold and follow the
[Hetzner guide](docs/providers/hetzner.md):

```sh
npx runneryard init --provider hetzner \
  --github https://github.com/acme/widgets
```

Hetzner support is preview quality until it has completed a public release
canary in a real Hetzner project. Keep the first workload low-risk and private.

Read the complete [quickstart](docs/quickstart.md) before routing production CI.

## Pick the next document

- New operator: follow the [quickstart](docs/quickstart.md).
- Fly or Hetzner: use the provider-specific [Fly](docs/providers/fly.md) or
  [Hetzner](docs/providers/hetzner.md) runbook.
- Sizing or changing a fleet: read the
  [configuration reference](docs/configuration.md) and
  [operations guide](docs/operations.md).
- Security review or removal: read the [security model](docs/security.md).
- New provider: implement the [adapter contract](docs/adapter-contract.md).
- Contributor or coding agent: read [AGENTS.md](AGENTS.md) and
  [CONTRIBUTING.md](CONTRIBUTING.md).

## Why it exists

GitHub-hosted runners are convenient, but high-volume repositories can pay for
the same clean VM repeatedly while long CI runs amplify every branch update.
Self-hosted autoscaling keeps GitHub's workflow UX while moving compute to an
account you own. `runneryard` is for teams that want that without operating a
Kubernetes cluster or giving a hosted runner vendor access to their code.

## How a job runs

1. GitHub assigns a queued job to a runner scale set.
2. The controller requests a one-time JIT configuration from GitHub.
3. A compute adapter starts a clean worker with that JIT value.
4. The worker accepts one job and exits.
5. The controller destroys it and reconciles any leaked infrastructure.

The public controller module has one operation, `Controller.Run`. Its compute
seam deliberately has only three:

```go
type Compute interface {
    Launch(context.Context, Lease) (Worker, error)
    Inventory(context.Context) ([]Worker, error)
    Destroy(context.Context, string) error
}
```

The core owns GitHub assignments, concurrency, deadlines, recovery, adoption,
and cleanup. Adapters own provider credentials, images, regions, machine shapes,
bootstrap, metadata, and lifecycle translation. See the
[adapter contract](docs/adapter-contract.md).

## Security defaults

- A worker receives only its one-time GitHub JIT configuration.
- Controller and worker infrastructure must be separate.
- The worker scope must contain zero permanent app secrets.
- Workers are one-job, auto-destroying, non-restarting instances.
- Fly job containers receive a validated public DNS policy so nested BuildKit
  steps cannot silently inherit an unreachable host-only resolver.
- `MAX_RUNNERS` is a hard concurrency ceiling.
- `RUNNER_MAX_LIFETIME` forces cleanup of hung or disconnected workers.
- `RUNNER_USAGE_BUDGET` is a durable rolling compute-time ceiling. New jobs
  queue when it is exhausted.
- Live capacity and cost limits are operator-owned configuration. A repository
  pull request must never raise them.
- Ownership metadata prevents one controller from deleting foreign machines.
- A dedicated GitHub App owned by the operator is the default. The setup flow
  requests no webhook and sends its key directly to controller storage.

Jobs execute repository code. Put workers in a cloud scope and network that
cannot reach production. Read the complete [security model](docs/security.md)
before deployment. Report vulnerabilities through the process in
[SECURITY.md](SECURITY.md).

## Distribution

| Form | Use |
| --- | --- |
| `npx runneryard` | Lowest-friction setup and diagnostics |
| GitHub Release binary | Node-free local and server operation |
| `ghcr.io/gwendall/runneryard` | Controller and worker runtime |
| Go module | Build a provider adapter or embed the controller |

The npm package contains no controller implementation. It downloads the exact
release matching its package version, verifies the published SHA-256 checksum,
caches it, and forwards arguments and process signals.

`runneryard route status|enable|disable` provides an explicit, idempotent switch
between a qualified scale-set label and `ubuntu-latest`. It uses the operator's
existing `gh` login; no GitHub token is copied into RunnerYard.

`runneryard status [--json]` reads a private, atomic controller snapshot with
capacity, two distinct latency measures, orphan candidates, and budget. It is
accessed through provider SSH; RunnerYard opens no monitoring port.

## Development

Requirements: Go 1.25+, Node 24+, pnpm 10, and Docker for image tests.

```sh
go test -race ./...
go vet ./...
corepack enable
pnpm install --frozen-lockfile
pnpm check
pnpm build
```

The website lives in `apps/web` and runs with `pnpm dev`. See
[AGENTS.md](AGENTS.md) and [CONTRIBUTING.md](CONTRIBUTING.md) before proposing a
change.

The release workflow publishes the versioned GHCR image before the matching
GitHub release. npm publishing uses OIDC trusted publishing and provenance;
there is no long-lived npm token in CI.

MIT licensed. See [LICENSE](LICENSE).
