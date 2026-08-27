# Security policy

## Supported versions

`runneryard` is pre-1.0. Security fixes are made on the latest release and on
the default branch. Older pre-1.0 releases may not receive patches.

## Report a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private
vulnerability reporting form:

https://github.com/gwendall/runneryard/security/advisories/new

Include the affected version or commit, deployment model, reproduction steps,
and potential impact. Do not include credentials, repository contents, or other
users' data. Maintainers will acknowledge a complete report as soon as
practical and coordinate remediation and disclosure with the reporter.

## Deployment boundary

Self-hosted runners execute repository code. Operators are responsible for
network isolation, provider credential scope, repository and runner-group
access, and excluding untrusted fork pull requests. The implementation's trust
model and safe defaults are documented in [docs/security.md](docs/security.md).
