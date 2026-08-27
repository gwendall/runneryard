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

Use a dedicated worker network. Do not peer it with production VPCs, databases,
internal dashboards, metadata services, or the controller. Egress to GitHub,
package registries, and explicitly required test services should be the only
default path.

## GitHub scope

Prefer a GitHub App installed only on selected repositories. Repository runner
scale sets require repository Administration access; Metadata read is implicit.
For organization fleets, use a dedicated runner group with selected-repository
access and the organization Self-hosted runners permission.

Do not expose self-hosted runners to public fork pull requests. A workflow from
a fork can execute arbitrary commands with the worker's network and filesystem
access even when no repository secret is provided.

## Provider scope

An adapter credential should be limited to one worker project, app, account, or
resource group. The controller tags every worker with its own controller ID and
ignores foreign inventory. `Destroy` is idempotent and only receives worker IDs
returned through the owned inventory path.

## Supply chain

The runtime pins the official GitHub Actions runner and Node 22 bootstrap images
by digest. The bootstrap runtime keeps shell-based planners compatible; jobs
that need a specific language version must still use their setup action.
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

This ceiling covers worker runtime, not the always-on controller, volumes,
network transfer, image storage, taxes, or provider price changes. Keep a
provider-side spending limit when one exists and calculate those fixed costs
separately.

## Outboarding

1. Route workflows back to a hosted runner label.
2. Stop the controller and verify worker inventory is empty.
3. Revoke the provider token and GitHub App private key.
4. Uninstall or delete the GitHub App.
5. Delete the worker and controller apps, projects, or resource groups.
6. Remove generated config and repository variables.

Deleting the npm package or the website is never required for outboarding; the
runtime has no hosted runneryard control plane.
