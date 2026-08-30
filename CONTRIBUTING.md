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
staticcheck ./...
gosec -quiet ./...
govulncheck ./...
pnpm check
pnpm build
bash -n controller-entrypoint runner-entrypoint runneryard-job-started.sh scripts/verify-runtime-toolcache.sh
git diff --check
```

Install the analysis tools once with `go install honnef.co/go/tools/cmd/staticcheck@v0.7.0`,
`go install github.com/securego/gosec/v2/cmd/gosec@v2.29.0`, and
`go install golang.org/x/vuln/cmd/govulncheck@v1.7.0`; CI pins the same versions.

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

Planned work is tracked in [docs/backlog.md](docs/backlog.md); open an issue
with the backlog identifier when you start an item.
