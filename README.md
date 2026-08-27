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

The public release supports this setup flow:

```sh
npx runneryard init --github https://github.com/acme/widgets
```

This creates three reviewable files for the default Fly provider and never
uploads a credential:

- `.runneryard/controller.env.example`
- `.runneryard/fly.controller.toml`
- `.github/workflows/runneryard-canary.yml`

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

Initialize the durable usage ledger once, deploy the controller, and trigger
the generated canary. A successful canary has three receipts: GitHub reports a
green job, the controller records the complete lifecycle, and provider
inventory returns to zero workers.

For Hetzner Cloud, generate its provider-specific scaffold and follow the
[Hetzner guide](docs/providers/hetzner.md):

```sh
npx runneryard init --provider hetzner \
  --github https://github.com/acme/widgets
```

Hetzner support is preview quality until it has completed a public release
canary in a real Hetzner project. Keep the first workload low-risk and private.

Read the complete [quickstart](docs/quickstart.md) before routing production CI.

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
- `MAX_RUNNERS` is a hard concurrency ceiling.
- `RUNNER_MAX_LIFETIME` forces cleanup of hung or disconnected workers.
- `RUNNER_USAGE_BUDGET` is a durable rolling compute-time ceiling. New jobs
  queue when it is exhausted.
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
[CONTRIBUTING.md](CONTRIBUTING.md) before proposing a change.

The release workflow publishes the versioned GHCR image before the matching
GitHub release. npm publishing uses OIDC trusted publishing and provenance;
there is no long-lived npm token in CI.

MIT licensed. See [LICENSE](LICENSE).
