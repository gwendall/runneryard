# Quickstart

This guide takes a private repository from no runner infrastructure to a
single, disposable canary. It intentionally starts at repository scope. Move to
an organization scale set only after isolation and cost controls are verified.

## 1. Choose a provider and generate the scaffold

Fly Machines is the available, production-piloted path:

```sh
npx runneryard init --github https://github.com/acme/widgets
```

Hetzner Cloud is a preview adapter:

```sh
npx runneryard init --provider hetzner \
  --github https://github.com/acme/widgets
```

Review every generated file. The initializer does not create cloud resources,
modify workflows other than its standalone canary, or collect credentials.

## 2. Create isolated infrastructure

Follow either [providers/fly.md](providers/fly.md) or
[providers/hetzner.md](providers/hetzner.md). Fly uses separate controller and
worker apps. Hetzner uses a dedicated project, a deny-inbound worker firewall,
and a persistent controller host. Do not deploy the controller yet.

## 3. Create credentials

Create a GitHub App owned by the target organization and install it only on the
repositories that should use the fleet. At repository scope it needs Metadata
read and Administration read/write so the controller can manage runner scale
sets. Store its client ID, installation ID, and private key only on the
controller.

For an initial private canary, a classic PAT accepted by GitHub's scale-set
client can be used locally. Do not persist a broad personal token in the cloud.

Create a credential scoped to the isolated worker provider boundary. It must be
able to create, list, and delete workers, but should have no access to
production applications. On Fly, use a worker-app deploy token. On Hetzner,
use a read/write token from a dedicated Cloud project.

## 4. Diagnose before deployment

```sh
npx runneryard doctor \
  --provider fly \
  --controller-app acme-ci-controller \
  --worker-app acme-ci-runners
```

Run this after both apps exist. Do not continue if control and worker scopes are
the same or if the worker app contains any secret. Explicitly initialize the
durable volume once with `runneryard budget init`; without an existing valid
ledger, the controller refuses to start.

For Hetzner:

```sh
npx runneryard doctor --provider hetzner --firewall-id 123456
```

Do not continue if the API cannot be reached or the worker firewall allows any
inbound traffic.

## 5. Deploy and trigger the canary

Deploy the selected bundled adapter using its generated controller
configuration. Other providers implement the [adapter contract](adapter-contract.md).

Commit the generated canary and run it with `workflow_dispatch`. Confirm all
three receipts:

1. GitHub reports a successful job on the scale-set label.
2. Controller logs show created, started, completed, and destroyed events for
   one runner.
3. Provider inventory contains no managed worker after completion.

## 6. Migrate gradually

Route one low-risk Linux job first. Keep hosted runners as a repository-variable
fallback during the pilot:

```yaml
runs-on: ${{ vars.CI_LINUX_RUNNER || 'ubuntu-latest' }}
```

Set `CI_LINUX_RUNNER` to the scale-set name after the canary passes. Keep macOS,
GPU, and workloads that rely on GitHub-hosted images on their current runners
until equivalent images are qualified.
