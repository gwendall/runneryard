# Hetzner Cloud adapter

The Hetzner adapter is a preview provider for disposable Cloud Servers. The
controller can run on any persistent Linux host. For each GitHub job it creates
one server from Hetzner's official `docker-ce` image, starts the RunnerYard
runtime container, and deletes the server after completion.

Preview means the adapter has automated coverage for creation, pagination,
ownership filtering, bootstrap isolation, and idempotent deletion. It still
needs a public release canary in a real Hetzner project before production use.
Start with a low-risk private repository.

## Isolation layout

Create a dedicated Hetzner Cloud project for the fleet. Do not connect it to a
production private network. Hetzner project API tokens are project-scoped, so a
separate project is the clearest blast-radius boundary.

The controller is trusted. It holds the Hetzner token, GitHub App key, and
durable budget ledger. Worker servers are untrusted. They receive only one
short-lived GitHub JIT configuration and a deadline.

## Prerequisites

Install Docker with Compose on the persistent controller host. Install and
authenticate the official `hcloud` CLI on the operator machine:

```sh
export HCLOUD_TOKEN='<dedicated project token>'
hcloud server list
```

Create the token in the Hetzner Console with read/write access to the dedicated
project. RunnerYard needs to create, list, and delete servers. Keep this token
only on the controller.

## Worker firewall

Create a firewall with no rules:

```sh
hcloud firewall create --name runneryard-workers
hcloud firewall describe runneryard-workers -o json | jq .id
```

Hetzner documents that no inbound rules block all inbound traffic, while no
outbound rules permit outbound traffic. The adapter requires the firewall ID
and attaches it during server creation. Do not add SSH, HTTP, or other inbound
rules to this firewall.

## Generate the repository scaffold

From the target GitHub checkout:

```sh
npx runneryard init \
  --provider hetzner \
  --region fsn1 \
  --github https://github.com/acme/widgets
```

This writes:

- `.runneryard/controller.env.example`
- `.runneryard/hetzner.controller.compose.yml`
- `.runneryard/.gitignore` for the completed environment and App key
- `.github/workflows/runneryard-canary.yml`

It does not create infrastructure or upload credentials. Copy the environment
example on the controller host and fill it there:

```sh
cp .runneryard/controller.env.example .runneryard/controller.env
chmod 600 .runneryard/controller.env
```

Create a dedicated GitHub App and write its verified credentials directly to
the generated, ignored files:

```sh
npx runneryard auth github create \
  --github https://github.com/acme/widgets \
  --sink file
```

This creates `.runneryard/github-app.pem` and `github-app.env` with mode
`0600`. Compose reads the non-secret IDs from the env file and mounts the key
read-only. No credential needs to be copied from a browser or flattened into a
command argument.

Required Hetzner values are:

```dotenv
COMPUTE_PROVIDER=hetzner
HCLOUD_TOKEN=<dedicated project token>
RUNNER_HETZNER_LOCATION=fsn1
RUNNER_HETZNER_SERVER_TYPE=cpx32
RUNNER_HETZNER_IMAGE=docker-ce
RUNNER_HETZNER_FIREWALL_ID=123456
RUNNER_IMAGE=ghcr.io/gwendall/runneryard:0.3.8
```

`RUNNER_HETZNER_NETWORK_ID` is optional. A Hetzner Cloud firewall does not
filter private-network traffic, so leave it empty unless the network is also
dedicated to untrusted CI workers.

## GitHub App

The generated GitHub App is private, subscribes to no webhook events, and is
owned by the selected user or organization. A repository scale set needs
Metadata read (implicit) and Repository Administration write. Install it only
on repositories that use this fleet. Use `auth github import` with a mode
`0600` PEM file to bring an existing app.

## Diagnose before deployment

Run the doctor with the worker firewall ID:

```sh
npx runneryard doctor \
  --provider hetzner \
  --firewall-id 123456
```

It checks the CLI, authenticated project access, and that the firewall has no
inbound rules. Do not continue after a failed check.

## Initialize and deploy the controller

The generated Compose file gives the controller durable local state. Initialize
the empty budget ledger exactly once:

```sh
docker compose -f .runneryard/hetzner.controller.compose.yml \
  run --rm controller budget init \
  --file /var/lib/runneryard/budget.json
```

The command refuses to replace an existing ledger. Then start the controller:

```sh
docker compose -f .runneryard/hetzner.controller.compose.yml up -d
docker compose -f .runneryard/hetzner.controller.compose.yml logs -f controller
```

Read the private status snapshot through the controller host rather than
opening an inbound metrics port:

```sh
docker compose -f .runneryard/hetzner.controller.compose.yml \
  exec controller /usr/local/bin/runneryard status
```

For an upgrade, first disable routing and stop the existing controller. Pin the
same release in both generated locations: `RUNNER_IMAGE` in
`.runneryard/controller.env` controls workers, while `image` in
`.runneryard/hetzner.controller.compose.yml` controls the controller. Preserve
the durable volume and stable `CONTROLLER_ID`, restart exactly one controller,
and do not initialize the ledger again. `status` proves the controller version;
set `RUNNERYARD_EXPECTED_VERSION` in the generated canary so the job itself
proves the worker image before routing normal jobs back.

The controller host can be a small Hetzner VM, another VPS, or an existing
isolated Docker host. It needs durable storage and outbound HTTPS to GitHub,
GHCR, and the Hetzner Cloud API. It never needs inbound access to worker VMs.
Choose capacity, lifetime, and the rolling budget with the
[configuration reference](../configuration.md). Keep those live limits under
operator control rather than allowing a repository pull request to raise them.

## Worker lifecycle

The adapter creates each server with:

- the official Hetzner `docker-ce` app image by default;
- the configured server type and location;
- a mandatory deny-inbound firewall;
- optional attachment to one dedicated private network;
- controller, lease, and runner ownership labels;
- public IPv4 and IPv6 for outbound CI traffic;
- cloud-init containing only a base64-encoded JIT lease and deadline.

The VM is created powered off. RunnerYard waits for every create and attachment
action, confirms that the required firewall is applied, and only then powers on
the VM. It also waits for the power-on action before accepting the worker.

Base64 is transport encoding, not encryption. The JIT configuration is the only
credential allowed on a worker and is valid for one runner registration. The
bootstrap creates the runtime container, erases the root-only host lease file
before starting it, and powers off after the container exits. The controller
remains responsible for deleting the server.

Use a snapshot ID through `RUNNER_HETZNER_IMAGE` if you need an immutable,
prequalified host image. Keep `RUNNER_IMAGE` pinned to a RunnerYard release.

## Canary receipts

Trigger the generated workflow and verify all three receipts:

1. GitHub reports a green job on the generated scale-set label.
2. Controller logs show created, started, completed, and destroyed.
3. `hcloud server list --selector runneryard-managed-by=true` returns no worker.

Hetzner bills a server until it is deleted, even when powered off. Treat a
powered-off worker as a leak and investigate it immediately.

## Outboarding

1. Run `runneryard route disable --github https://github.com/acme/widgets` and
   verify `route status` reports `ubuntu-latest`.
2. Stop the controller after active jobs finish.
3. List and delete all servers carrying RunnerYard ownership labels.
4. Revoke the Hetzner project token.
5. For a dedicated App, revoke its keys and uninstall/delete it. For a shared
   BYO App, remove this controller's local credential first. Change repository
   access only after confirming no remaining consumer needs the installation
   for that repository. For an organization fleet, retire its scale set and
   runner-group access as applicable without treating the shared organization
   installation as controller-owned. Do not revoke shared keys, uninstall the
   installation, or delete the App while another consumer uses it.
6. Remove the dedicated project, firewalls, controller, and generated secrets.
