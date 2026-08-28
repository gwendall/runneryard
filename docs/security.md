# Security model

`runneryard` assumes workflow jobs may execute hostile code. A compromised
worker must not yield a permanent GitHub credential, a provider credential, or
network access to production.

## Trust boundaries

The controller is trusted. It holds the GitHub App private key and a narrowly
scoped provider credential. Workers are untrusted and disposable. They receive
only GitHub's just-in-time runner configuration for one job.

Controller and workers must not share an app-level secret scope. On Fly, every
Machine in an app inherits that app's secrets, so the adapter requires separate
controller and worker apps. The CLI doctor treats any worker-app secret as a
failure. Every worker process also sets `ignore_app_secrets=true`, so a future
operator mistake cannot inject worker-app secrets into job code.

On Hetzner Cloud, use a dedicated project and attach a firewall with no inbound
rules to every worker. The project token stays on the controller. Cloud-init
receives only the short-lived JIT configuration and deadline, encoded into a
root-only lease file. Encoding is not encryption. The bootstrap creates the
runner container, erases the host lease file before starting it, and shuts down
the VM when the container exits. Never add a permanent credential to user data.

Use a dedicated worker network. Do not peer it with production VPCs, databases,
internal dashboards, metadata services, or the controller. Egress to GitHub,
package registries, and explicitly required test services should be the only
default path.

Fly workers pass a non-secret, validated list of public resolver IPs to their
nested Docker daemon because Fly's host-side private resolver is not reliably
reachable from the inner bridge. RunnerYard accepts only one to three literal
IP addresses and validates the value at both the trusted controller and
untrusted worker boundary. Do not point this setting at a production or
internal resolver: job code controls its DNS queries, and the worker network
must remain unable to reach private services.

## GitHub scope

Prefer a GitHub App installed only on selected repositories. Repository runner
scale sets require repository Administration access; Metadata read is implicit.
For organization fleets, use a dedicated runner group with selected-repository
access and the organization Self-hosted runners permission.

Do not expose self-hosted runners to public fork pull requests. A workflow from
a fork can execute arbitrary commands with the worker's network and filesystem
access even when no repository secret is provided.

### GitHub App ownership

The safest self-hosted default is one private GitHub App owned by the operator
and installed only on the repositories served by that fleet. RunnerYard's
manifest flow runs through a random-state callback bound to `127.0.0.1`, has a
maximum one-hour lifetime, requests no webhook events, and asks only for the
runner-management permission required by repository or organization scope.
The returned installation is verified before any credential is stored.
[GitHub documents the manifest handshake, its one-hour limit, and the `state`
protection](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest).

The one-time private key is never printed. The Fly sink sends a triple-quoted
secret document to `fly secrets import` over stdin, not in process arguments.
The file sink refuses symlinked credential paths, writes files and its
directory with private modes, and adds those files to `.runneryard/.gitignore`.

There is intentionally no shared RunnerYard GitHub App. Giving every
self-hosted controller the private key of one central app would let any
controller sign as that app and attempt to reach other installations. A
central app is safe only when a hosted credential broker retains the private
key, authorizes each installation, mints short-lived tokens, and is operated as
a security-sensitive control plane. RunnerYard does not currently run that
service.

Bring-your-own mode accepts a client or app ID, installation ID, and a private
key file. It verifies the app identity, target account, installation, and
required permission before passing the credential to the same sinks. A private
key file readable by group or others, or any symlinked key path, is rejected.

### Rotation

Create a second GitHub App private key before removing the first. Run
`runneryard auth github import --force` with the new mode `0600` PEM and the
same installation. The Fly sink imports all three values as one update. The
file sink backs up the existing credential pair during replacement and
restores it if either new file cannot be installed. Deploy or restart the
controller, run the canary, then revoke the old key in GitHub.

## Provider scope

An adapter credential should be limited to one worker project, app, account, or
resource group. The controller tags every worker with its own controller ID and
ignores foreign inventory. `Destroy` is idempotent and only receives worker IDs
returned through the owned inventory path.

Hetzner Cloud firewalls protect public networking only. Do not attach workers
to a private network that can reach production. A separate Cloud project is the
safest default boundary.

## Supply chain

The runtime pins the Ubuntu base, official GitHub Actions runner, and Node 22
bootstrap images by multi-architecture digest. Docker and Buildx package
versions are explicit. The worker selects `fuse-overlayfs` only when its Docker
data root is already overlay-backed; otherwise it retains Docker's native
snapshotter. It fails closed if that layered path lacks `/dev/fuse`, and never
falls back to the deep-copy `vfs` driver. The bootstrap runtime keeps shell-based planners compatible and
publishes that same pinned Node version through the GitHub runner toolcache on
both x64 and arm64; release qualification executes the entrypoint and offline
`setup-node` contract on both architectures before publishing one manifest.
Workflows should still use `actions/setup-node`: the pinned
patch resolves locally, while a floating major or any other requested version
may follow the action's normal managed download path. Operators should update
the digest-pinned runtime first, then pin repository workflows to that exact
patch; a pull request must never choose the worker image itself.
Release binaries ship with SHA-256 checksums; the npm launcher verifies them
before execution. GitHub Actions dependencies are pinned to commit SHAs. Public
npm publishing is configured for OIDC trusted publishing and provenance,
without a long-lived npm token.

## Cost safety is security

`MAX_RUNNERS` bounds concurrent compute. `RUNNER_MAX_LIFETIME` is enforced in
the worker and removes jobs that never emit a completion event. Before launch,
the controller durably reserves that worst-case lifetime against
`RUNNER_USAGE_BUDGET`; completion replaces the reservation with actual elapsed
time, capped at the reservation because the worker deadline includes its
30-second shutdown grace. Unknown launch outcomes keep the full reservation.
When the rolling budget is exhausted, new GitHub jobs queue instead of
starting compute. The ledger must live on durable controller storage and fails
closed if it cannot be read or written. `MIN_RUNNERS` defaults to zero.

Retirement is also journaled before destructive work. The controller deletes a
GitHub runner registration only after provider absence is authoritative, only
for a controller-generated `runner-*` identity, and only when its immutable
registration ID, scale-set ID, and provider lease proof agree. That proof is
stored both in the private retirement journal and in provider metadata, so a
restart or a same-name replacement cannot widen cleanup authority. Cleanup
credentials never reach workers. A typed GitHub `job still running` response
keeps that exact proof pending and does not stop the listener; later
reconciliation may retry it, but may not substitute a name-matched registration
with a different ID. Every other identity mismatch still fails closed.

This ceiling covers worker runtime, not the always-on controller, volumes,
network transfer, image storage, taxes, or provider price changes. Keep a
provider-side spending limit when one exists and calculate those fixed costs
separately.

## Outboarding

1. Route workflows back to a hosted runner label.
2. Stop the controller and verify worker inventory is empty.
3. Revoke the provider token.
4. If the fleet owns a dedicated GitHub App, revoke its keys, uninstall it, and
   delete it. For a shared BYO App, remove only this controller's local
   credential first. Inventory every remaining App consumer before changing
   the shared installation: remove repository access only when no other
   consumer needs that installation for the repository. For an
   organization-scoped fleet, retire its scale set and runner-group access as
   applicable; do not treat the organization installation as controller-owned.
   Never revoke a shared key, uninstall a shared installation, or delete a
   shared App until every remaining consumer has rotated away from it.
5. Delete the worker and controller apps, projects, or resource groups.
6. Remove generated config, local credential files, and repository variables.

Deleting the npm package or the website is never required for outboarding; the
runtime has no hosted runneryard control plane.
