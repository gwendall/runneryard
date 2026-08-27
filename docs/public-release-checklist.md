# Public release checklist

Use this checklist before changing repository visibility or publishing a tag.

## Repository

- [ ] Review the complete Git history, not only the current tree, for secrets,
  private deployment names, internal repository links, personal data, and large
  generated files. Rewrite or replace the history before publication if needed.
- [ ] Enable branch protection for `main` and require the CI test job.
- [ ] Enable private vulnerability reporting, secret scanning, push protection,
  and Dependabot security updates.
- [ ] Disable the wiki unless it will be maintained.
- [ ] Confirm the repository description, topics, license, issue templates, and
  security contact render correctly while signed out.

## Distribution

- [ ] Make the GHCR package public and verify an anonymous image pull.
- [ ] Configure npm trusted publishing for the `publish-npm` job in
  `.github/workflows/release.yml`.
- [ ] Confirm the `runneryard` package name is still available on npm.
- [ ] Tag `v<version>` from a fully qualified commit.
- [ ] Verify every release checksum and all four binary targets.
- [ ] Run `npx runneryard@<version> version` on macOS and Linux from an empty npm
  cache.
- [ ] Verify the published image contains GitHub CLI, Node, Git, Git LFS, Docker,
  and the pinned Actions runner.

## First-user journey

- [ ] Follow `docs/quickstart.md` from a fresh GitHub repository and a fresh
  provider account.
- [ ] Confirm each provider's `init` command creates only the files documented in its guide.
- [ ] Confirm Fly `doctor` rejects a shared controller/worker app and any
  worker-app secret.
- [ ] Confirm Hetzner `doctor` rejects missing or inbound-permitting firewalls.
- [ ] Run a canary on every bundled provider and prove the worker disappears
  from provider inventory.
- [ ] Exercise the rolling usage budget and confirm new jobs queue after it is
  exhausted.
- [ ] Complete every outboarding step in `docs/security.md`.

## Website

- [ ] Deploy `apps/web` over HTTPS and set the repository homepage URL.
- [ ] Check the production page signed out on desktop and mobile.
- [ ] Verify the quickstart, source, security, and provider-documentation links.
- [ ] Run Lighthouse accessibility, performance, best-practices, and SEO checks.
