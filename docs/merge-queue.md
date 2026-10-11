# Merge queue (preview)

`runneryard merge-queue tick` is a merge queue that runs as a GitHub Actions
workflow in the repository it serves, with its own dedicated GitHub App. It is
for repositories where GitHub's native merge queue is unavailable (it needs a
ruleset or branch protection, which GitHub Free does not offer on a private
repository) and where the operator does not want to give a hosted queue access
to the code.

It is a preview: the commands, the configuration file and the comment format
may still change between releases. It shares nothing with the runner
controller: it never runs inside it and never uses its credentials, and a
fleet that does not enable it is unaffected.

## What it does

A pull request that passes CI on its own branch can still break the target
branch once merged, because something else landed in between. The queue
tests pull requests on top of the current target, and moves the target only
to a commit whose required checks passed.

1. Someone with write access adds the `queue` label to a pull request.
2. The queue takes up to `batch_size` labeled pull requests, oldest label
   first, and stacks them: the first one's own commits are rebased on the
   target, the second one's on the first, and so on. It force-pushes each
   pull request branch with a lease on the head it saw, all in one atomic
   push.
3. The push starts CI on every branch of the stack. Only the top of the
   stack contains every change, so the queue cancels the `pull_request`
   runs of the other members and waits for the required checks on the top.
4. When they pass, the queue fast-forwards the target to the top of the
   stack with a plain, non-force push. The target either still is the base
   the stack was tested on, or the push is refused. GitHub then marks every
   pull request of the stack as merged, because its head is now part of the
   target.
5. When a check fails, the queue re-runs the failed jobs once, to absorb a
   flaky test. If it fails again, a single pull request is rejected (label
   removed, comment with links to the failing checks); a stack of several is
   split in two halves stacked again on the same base, which finds the
   culprit in at most log2(`batch_size`) rounds.

There is one lane: the queue forms a new stack only when nothing is in
flight, except for the two halves of a split. A rebase conflict rejects the conflicting pull request at once,
since running it again cannot help. A required check that never reports
fails the stack after `check_timeout`.

Each run of the workflow is one **tick**: read the queue from GitHub, decide,
act, exit. The queue has no database. Its state is the labels, the branches,
and one status comment per pull request, edited in place, that carries hidden
markers:

- on the top of a stack: the base it was tested on, each member's head and
  the retry count;
- on each member: the commits that are its own, so a member that leaves a
  split or abandoned stack is stacked again without its former batchmates'
  commits;
- after landing: each member's own head as it landed, so the merge
  detection lag on GitHub's side does not queue a landed pull request again.

A stack is abandoned, and its members queued again, when the target moves
outside the queue, a member is pushed to, closed or unlabeled, or another
stack lands first. Abandoning cancels the stack's CI.

While a stack is in flight, the diff of each member above the first also
shows the members below it, since its branch contains them. The queue
rewrites the commits it stacks: authors are kept, the committer becomes
`runneryard-merge-queue`, and signatures are dropped.

## Configuration

The queue reads `.github/merge-queue.yml` from the default branch on every
tick, so a pull request cannot change the rules it is judged by. Unknown keys
are an error. See [examples/merge-queue-config.yml](examples/merge-queue-config.yml).

| Key | Default | Meaning |
| --- | --- | --- |
| `target` | `main` | Branch the queue fast-forwards. |
| `label` | `queue` | Label that queues a pull request. |
| `batch_size` | `3` | Pull requests per stack, 1 to 10. |
| `check_timeout` | `45m` | How long a required check may be pending or absent. |
| `required_checks` | none, required | Check run names that must pass, each optionally limited to `paths`. |

A check passes with the conclusion `success`, `neutral` or `skipped`, and the
newest run of each name counts. A check with `paths` is only required when the
stack changes a matching file: `dir/**` matches everything under `dir`,
other patterns follow Go's `filepath.Match`, where `*` stays within one
directory.

## Operator setup

### 1. Create the queue's GitHub App

Create a dedicated App, separate from the runner App, under **Settings,
Developer settings, GitHub Apps, New GitHub App** of the account that owns
the repository:

- Name: anything, for example `acme-merge-queue`. Its bot login,
  `<app-slug>[bot]`, is the only author whose markers the queue trusts.
- Homepage URL: the repository URL. Webhook: inactive, the workflow is the
  trigger.
- Repository permissions:

  | Permission | Access | Used to |
  | --- | --- | --- |
  | Contents | Read and write | read the configuration, push stack branches, fast-forward the target |
  | Pull requests | Read and write | read pull requests, comment on them |
  | Issues | Read and write | list labeled pull requests, edit the status comment, remove the label |
  | Actions | Read and write | find, cancel and re-run workflow runs |
  | Checks | Read | read check runs on the top of a stack |
  | Metadata | Read | mandatory; read collaborator permissions |
  | Workflows | Read and write | only if queued pull requests may change `.github/workflows` |

- Where can this App be installed: only on this account.

Install it on the one repository the queue serves, not on all repositories.
Generate a private key, then in the repository's **Settings, Secrets and
variables, Actions**:

- variable `MERGE_QUEUE_APP_CLIENT_ID`: the App's client ID;
- secret `MERGE_QUEUE_APP_PRIVATE_KEY`: the private key.

To keep the key from any job that does not need it, put both in an
environment limited to the default branch and reference it from the `tick`
job.

### 2. Create the label

Create the `queue` label (or the `label` you configured). Any label works; the
queue only acts on pull requests whose latest labeling was done by someone
with write or admin access.

### 3. Add the configuration and the workflow

Copy [examples/merge-queue-config.yml](examples/merge-queue-config.yml) to
`.github/merge-queue.yml` and list your required checks: the check run names
your CI reports, usually the job names. Copy
[examples/merge-queue.yml](examples/merge-queue.yml) to
`.github/workflows/merge-queue.yml` and:

- set `RUNNERYARD_VERSION` to an exact release;
- replace `"CI"` under `workflow_run` with the name of the workflow that
  reports the required checks;
- replace `main` under `push` with the target;
- keep the label in `QUEUE_LABEL` and in the `gate` job's `if:` in sync
  with the configuration.

Your CI must run on `pull_request` events, including `synchronize`, which is
the default: that is how a stack gets tested.

Events drive the ticks: a change of the queue label, a CI run finishing on a
pull request, and the queue's own push to the target, which forms the next
stack as soon as one lands. The schedule is only a safety net for missed
events and check timeouts. On a private repository each scheduled run bills
the `gate` job one minute, about 1,440 minutes a month at every 30 minutes,
so widen the interval if minutes matter.

The workflow has two jobs. `gate` starts no runner at all for an event that is
not about the queue (another label, a CI run on a push to the target), and
makes one `gh pr list` call without a checkout for the others. `tick` only
starts when something is queued. Ticks never overlap: they share one
concurrency group.

### 4. Let the App move the target

If the target is protected by a ruleset or branch protection, the App must be
allowed to push to it directly: add it to the bypass list. A rule that
requires signed commits cannot be met, since the queue rebases. A rule that
requires linear history is met: the target only ever fast-forwards.

### 5. Start with a dry run

Until the App variable and secret exist, the workflow runs the tick with the
read-only `GITHUB_TOKEN`, which only prints the plan in the job log. Label a
pull request and read the log of the `Merge queue` workflow. The same works
locally:

```sh
GITHUB_TOKEN=$(gh auth token) runneryard merge-queue tick --repo acme/widgets --dry-run
```

Add `--bot-login acme-merge-queue[bot]` to see stacks already in flight.

## Security

- **A separate trust boundary.** The queue has its own App and runs in an
  Actions job of the repository it serves. It never runs in the runner
  controller and never reads the controller's credentials, budget ledger or
  provider inventory. Its token is an installation token for one repository,
  minted per run, which expires within the hour.
- **No pull request code runs with the App's token.** The workflow is
  triggered by `pull_request_target`, `workflow_run`, `schedule`,
  `workflow_dispatch` and pushes to the target, which all run the workflow
  file of the default branch (or of the target, for a push),
  and no job checks out pull request code. Do not switch the trigger to
  `pull_request`. The queue itself only runs git: hooks, the user and system
  git configuration, `core.fsmonitor` and LFS smudging are disabled, and git
  inherits only `PATH`, `HOME`, locale, certificate and proxy variables from
  the job.
- **The token stays out of git's storage.** It reaches git as an HTTP header
  in the environment of each git call, over https only; never in a URL, an
  argument or `.git/config`. The clone is deleted after the tick.
- **Who can queue.** Labels can be added by triage users, so the queue checks
  the permission of whoever added the label last, not of the author, and
  requires write or admin. Pull requests from forks, drafts and pull requests
  whose base is not the target are refused with a comment.
- **Who can steer the queue.** Markers are read only from comments authored
  by the App's own bot account. A comment by anyone else that copies a marker
  is ignored.
- **Pushes are compare-and-swap.** Branches are pushed with a lease on the
  head the tick saw, atomically, so an author's push in between wins and the
  stack is formed again later. The target moves only by a non-force push of
  the tested commit.
- **What a green check means.** The queue trusts check runs as GitHub reports
  them on the top of the stack. A pull request that edits the CI workflow is
  tested by its own edited workflow, as with GitHub's native queue, so review
  workflow changes before labeling them. A check run of the same name created
  by another App with `checks: write` on the repository counts too.
- **Reviews are not enforced.** The label of a user with write access is the
  only gate. Repositories that need approvals must keep enforcing them in
  their review process.
- **Commits are rewritten.** Signatures are dropped and the committer
  changes, so a repository that requires signed commits cannot use this
  version.

## Failure modes

| Situation | What the next tick does |
| --- | --- |
| The target moved outside the queue | Abandons the stack, cancels its CI, stacks again on the new target. |
| A member was pushed to, closed or unlabeled | Abandons the stack; the others are stacked again with their own commits only. |
| A branch moved between snapshot and push | Pushes nothing, forms the stack on a later tick. |
| The target already contains the stack, after a crash | Records the stack as landed instead of testing it again. |
| GitHub has not yet marked a landed pull request merged | Skips it: its landed marker matches its head. |
| A check never reports | Fails the stack after `check_timeout`, then splits or rejects. |
| A failed run cannot be re-run | Splits or rejects without waiting for a re-run. |
| `.github/merge-queue.yml` is missing or invalid | Fails the tick with the parse error; nothing is written. |

## Not built yet

- Squash or merge-commit landing, and keeping commit signatures.
- Speculative stacks tested in parallel, priorities, a freeze switch, and
  independent lanes per path.
- Testing on dedicated branches instead of rewriting pull request branches.
- Limiting required checks to a given App, such as GitHub Actions.
- Requiring an approving review before a pull request is stacked.
