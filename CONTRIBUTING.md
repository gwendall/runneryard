# Contributing

Thanks for helping improve `runneryard`. Read [AGENTS.md](AGENTS.md) first.
Changes should preserve the small provider interface, explicit trust
boundaries, and fail-closed cost controls.

## Set up

Create an isolated branch or worktree from the latest `main`, then install the
workspace dependencies:

```sh
git fetch origin main
git worktree add ../runneryard-change -b change/my-change origin/main
cd ../runneryard-change
corepack enable
pnpm install --frozen-lockfile
```

## Validate

Run the same checks expected in CI:

```sh
go test -race ./...
go vet ./...
pnpm check
pnpm build
bash -n controller-entrypoint runner-entrypoint scripts/verify-runtime-toolcache.sh
git diff --check
```

Add tests for behavior changes. Provider adapters must satisfy the invariants in
[docs/adapter-contract.md](docs/adapter-contract.md), including ownership
filtering, secret isolation, idempotent deletion, and partial-create cleanup.

## Pull requests

- Keep each pull request focused on one change.
- Record the exact qualified head SHA and avoid invalidating it for disjoint
  changes on `main`.
- Explain the trust-boundary and cost impact when either changes.
- Pin new GitHub Actions dependencies to a full commit SHA.
- Never commit credentials, completed environment files, private keys, provider
  state, or names and URLs copied from a private deployment.
- Update operator documentation when configuration or failure modes change.

Security reports belong in the private channel described in
[SECURITY.md](SECURITY.md), not in a public issue.
