# Repository agent contract

RunnerYard is a security-sensitive controller for disposable GitHub Actions
workers. Preserve its small provider seam, explicit trust boundaries, and
fail-closed cost controls.

## Before changing code

1. Fetch `origin/main` and work from a dedicated branch or worktree.
2. Read `README.md`, then the task-specific document under `docs/`.
3. Treat generated configuration, deployment names, credentials, provider
   inventory, and durable controller state as private operator data.

Never put a permanent credential in a worker, widen provider deletion beyond
owned inventory, reset a budget ledger, or let a pull request raise an
operator-controlled limit such as `MAX_RUNNERS`.

## Qualification and integration

- Qualify one immutable commit. Record its exact SHA in the pull request.
- If `main` advances, use `git diff --name-only <base>..<head>` and
  `<base>..origin/main` to check semantic overlap. Do not rebase a qualified,
  disjoint head merely to become zero commits behind.
- Re-integrate and requalify only for a conflict, shared file, changed
  dependency, or other real overlap. Let the protected merge lane serialize
  otherwise independent work.
- Review behavior against the issue/spec and repository standards separately.
- Never close and reopen a pull request to retrigger CI. Rerun only the failed
  or intentionally invalidated checks.

Run the validation commands in `CONTRIBUTING.md`. Security, cleanup, budget, or
provider lifecycle changes require focused regression tests and updated
operator documentation.
