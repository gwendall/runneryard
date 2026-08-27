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

The recommended flow creates a private GitHub App owned by the target user or
organization. For Fly, its one-time private key goes from GitHub directly to
the controller app secret store:

```sh
npx runneryard auth github create \
  --github https://github.com/acme/widgets \
  --controller-app acme-ci-controller
```

For a Docker controller, including Hetzner, use the local file sink:

```sh
npx runneryard auth github create \
  --github https://github.com/acme/widgets \
  --sink file
```

The file sink creates `.runneryard/github-app.pem` and
`.runneryard/github-app.env` with mode `0600` and adds both to the generated
ignore file. The Compose scaffold mounts the key read-only.

The CLI opens a loopback setup page, then GitHub shows the app owner and exact
permission before approval. A repository fleet needs Repository
Administration write because GitHub places runner scale-set management behind
that permission; Metadata read is implicit. An organization fleet instead
needs Organization Self-hosted runners write. The app is private, subscribes
to no webhook events, and should be installed only on selected repositories.

RunnerYard does not operate a hosted credential service. The temporary
manifest code and private key return to the local CLI, are verified against the
selected installation, and move to the chosen secret sink without being
printed. See the [security model](security.md#github-app-ownership).

To bring an existing app, keep the PEM in a mode `0600` file and run:

```sh
npx runneryard auth github import \
  --github https://github.com/acme/widgets \
  --client-id Iv1.example \
  --installation-id 123456 \
  --private-key-file ./existing-app.pem \
  --controller-app acme-ci-controller
```

The import path verifies the app identity, installation owner, repository
access, and permission before storing anything. A personal token remains a
compatibility escape hatch for a private canary, but `doctor` warns about it.

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
the same, the GitHub App secret set is incomplete, or the worker app contains
any secret. Explicitly initialize the
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
