package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Report is what one tick did.
type Report struct {
	Formed    [][]int
	Landed    [][]int
	Retried   [][]int
	Split     [][]int
	Abandoned [][]int
	Rejected  []int
	// Deferred lists stacks not formed because a branch or the target moved
	// during the tick; the next tick plans them again.
	Deferred [][]int
	Idle     bool
}

// Options configures one tick.
type Options struct {
	GitHub GitHub
	// OpenGit clones the repository with the target branch checked out. A
	// tick only calls it when its plan needs git, and never in a dry run.
	OpenGit func(ctx context.Context, target string) (Git, error)
	// BotLogin is the queue's own account, "<app-slug>[bot]": only its
	// comments carry trusted markers. A tick that writes requires it.
	BotLogin string
	// DryRun plans and prints, and writes nothing.
	DryRun bool
	// SettleWait is how long a tick that formed a stack keeps cancelling
	// the batchmate runs its push started. Zero leaves them to later ticks.
	SettleWait time.Duration
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// Tick runs one merge queue tick: it reads the configuration from the
// default branch, takes a snapshot, plans, and applies the plan unless
// DryRun is set.
func Tick(ctx context.Context, options Options) (Plan, Report, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logf == nil {
		options.Logf = func(string, ...any) {}
	}
	if !options.DryRun && options.BotLogin == "" {
		return Plan{}, Report{}, errors.New("a tick that writes needs the queue's bot login, to recognize its own status comments")
	}
	data, err := options.GitHub.ReadFile(ctx, ConfigPath)
	if err != nil {
		return Plan{}, Report{}, fmt.Errorf("read %s from the default branch: %w", ConfigPath, err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		return Plan{}, Report{}, err
	}
	snapshot, err := LoadSnapshot(ctx, options.GitHub, config, options.BotLogin, options.Now().UTC())
	if err != nil {
		return Plan{}, Report{}, err
	}
	plan := PlanTick(snapshot, config)
	if options.DryRun {
		return plan, Report{Idle: plan.Idle}, nil
	}
	report, err := Apply(ctx, plan, snapshot, config, options)
	return plan, report, err
}

// Apply carries out a plan. It is the only place that runs git or writes to
// GitHub.
func Apply(ctx context.Context, plan Plan, snapshot Snapshot, config Config, options Options) (Report, error) {
	a := &applier{options: options, config: config, snapshot: snapshot, queued: map[int]QueuedPR{}}
	if a.options.Logf == nil {
		a.options.Logf = func(string, ...any) {}
	}
	if a.options.Now == nil {
		a.options.Now = time.Now
	}
	for _, pr := range snapshot.Queued {
		a.queued[pr.Number] = pr
	}
	a.report.Idle = plan.Idle
	for _, action := range plan.Actions {
		if err := a.apply(ctx, action); err != nil {
			return a.report, fmt.Errorf("%s: %w", action, err)
		}
	}
	return a.report, nil
}

type applier struct {
	options  Options
	config   Config
	snapshot Snapshot
	queued   map[int]QueuedPR
	git      Git
	report   Report
}

func (a *applier) openGit(ctx context.Context) (Git, error) {
	if a.git == nil {
		git, err := a.options.OpenGit(ctx, a.config.Target)
		if err != nil {
			return nil, fmt.Errorf("clone the repository: %w", err)
		}
		a.git = git
	}
	return a.git, nil
}

func (a *applier) comment(ctx context.Context, number int, body string) error {
	return a.options.GitHub.UpsertStatusComment(ctx, number, a.options.BotLogin, body)
}

func (a *applier) apply(ctx context.Context, action Action) error {
	switch action.Kind {
	case ActionComment:
		return a.comment(ctx, action.Number, action.Body)
	case ActionCancel:
		a.cancel(ctx, action.RunIDs)
	case ActionForm:
		return a.form(ctx, action.PRs, action.Base)
	case ActionAdvance:
		return a.advance(ctx, action.Stack)
	case ActionLanded:
		return a.landed(ctx, action.Stack)
	case ActionRetry:
		// A run that cannot be re-run leaves its failure older than the
		// retry, which counts as pending until the check timeout, then
		// splits or rejects the stack as usual.
		for _, id := range action.RunIDs {
			if err := a.options.GitHub.RerunFailedJobs(ctx, id); err != nil {
				a.options.Logf("could not re-run the failed jobs of run %d: %v", id, err)
			}
		}
		for i, member := range action.Stack.Members {
			keep := keepOwns(a.queued[member.Number].Owns, stackOwn(action.Stack, i))
			if err := a.comment(ctx, member.Number, stackComment(action.Stack, i, keep)); err != nil {
				return err
			}
		}
		a.report.Retried = append(a.report.Retried, action.Stack.Numbers())
	case ActionReject:
		return a.reject(ctx, action.Number, action.Reason, action.URLs)
	case ActionSplit:
		a.report.Split = append(a.report.Split, action.Stack.Numbers())
	case ActionAbandon:
		a.cancelPullRequestRuns(ctx, action.Stack.Top().SHA)
		for _, number := range action.PRs {
			pr, queued := a.queued[number]
			if !queued {
				continue
			}
			if err := a.comment(ctx, number, requeuedComment(action.Reason, pr.Owns)); err != nil {
				return err
			}
		}
		a.report.Abandoned = append(a.report.Abandoned, action.Stack.Numbers())
	}
	return nil
}

// form rebases prs on top of each other above base, records each member's
// own commits in its status comment, then pushes every branch in one atomic
// push, each leased on the head the snapshot saw. A rebase conflict rejects
// a single pull request or splits the group at once: it is deterministic,
// so there is nothing to retry.
func (a *applier) form(ctx context.Context, prs []int, base string) error {
	git, err := a.openGit(ctx)
	if err != nil {
		return err
	}
	target, err := git.Fetch(ctx, "refs/heads/"+a.config.Target)
	if err != nil {
		return err
	}
	if target != base {
		a.postpone(prs, "the target branch moved")
		return nil
	}
	tip := base
	stack := Stack{Base: base, At: a.snapshot.Now}
	var updates []BranchUpdate
	var keeps [][]Own
	for _, number := range prs {
		pr := a.queued[number]
		head, err := git.Fetch(ctx, "refs/heads/"+pr.HeadRef)
		if err != nil {
			return err
		}
		if head != pr.HeadSHA {
			a.postpone(prs, fmt.Sprintf("#%d moved", number))
			return nil
		}
		upstream, used := a.upstream(ctx, git, pr, base)
		rebased, err := git.Rebase(ctx, tip, upstream, head)
		if errors.Is(err, ErrRebaseConflict) {
			return a.conflict(ctx, prs, base, number)
		}
		if err != nil {
			return err
		}
		stack.Members = append(stack.Members, Member{Number: number, SHA: rebased})
		updates = append(updates, BranchUpdate{Branch: pr.HeadRef, SHA: rebased, Expected: pr.HeadSHA})
		keeps = append(keeps, used)
		tip = rebased
	}
	// Comments first, the top one (with the stack marker) last: if the push
	// below never happens, each comment still holds the record of its branch
	// as it is, and the next tick abandons the stack marker.
	for i, member := range stack.Members {
		if err := a.comment(ctx, member.Number, stackComment(stack, i, keeps[i])); err != nil {
			return err
		}
	}
	if err := git.PushBranches(ctx, updates); errors.Is(err, ErrLeaseRejected) {
		a.postpone(prs, "a branch moved")
		return nil
	} else if err != nil {
		return err
	}
	a.report.Formed = append(a.report.Formed, prs)
	a.options.Logf("formed a stack of %s, top %s", refs(prs), shortSHA(tip))
	a.settle(ctx, stack)
	return nil
}

// upstream picks the commit below a pull request's own commits: the newest
// own-commits record that still describes its branch, else the target. It
// also returns that record, kept in the next status comment.
func (a *applier) upstream(ctx context.Context, git Git, pr QueuedPR, base string) (string, []Own) {
	for _, own := range pr.Owns {
		headOK, err := git.IsAncestor(ctx, own.Head, pr.HeadSHA)
		if err != nil || !headOK {
			continue
		}
		upstreamOK, err := git.IsAncestor(ctx, own.Upstream, own.Head)
		if err != nil || !upstreamOK {
			continue
		}
		return own.Upstream, []Own{own}
	}
	return base, nil
}

func (a *applier) conflict(ctx context.Context, prs []int, base string, culprit int) error {
	if len(prs) == 1 {
		return a.reject(ctx, culprit, "it does not rebase cleanly onto the target", nil)
	}
	front, back := Split(prs)
	a.report.Split = append(a.report.Split, prs)
	if err := a.form(ctx, front, base); err != nil {
		return err
	}
	return a.form(ctx, back, base)
}

func (a *applier) reject(ctx context.Context, number int, reason string, urls []string) error {
	if err := a.options.GitHub.RemoveLabel(ctx, number, a.config.Label); err != nil {
		return err
	}
	if err := a.comment(ctx, number, rejectedComment(reason, a.config.Label, urls, a.queued[number].Owns)); err != nil {
		return err
	}
	a.report.Rejected = append(a.report.Rejected, number)
	return nil
}

// advance fast-forwards the target to the top commit with a plain push: it
// fails, writing nothing, if the target moved since the snapshot. GitHub
// then marks every member merged, since each head is now on the target.
func (a *applier) advance(ctx context.Context, stack Stack) error {
	git, err := a.openGit(ctx)
	if err != nil {
		return err
	}
	top := stack.Top()
	head, err := git.Fetch(ctx, "refs/heads/"+a.queued[top.Number].HeadRef)
	if err != nil {
		return err
	}
	if head != top.SHA {
		a.postpone(stack.Numbers(), fmt.Sprintf("#%d moved", top.Number))
		return nil
	}
	if err := git.FastForward(ctx, a.config.Target, top.SHA); errors.Is(err, ErrNotFastForward) {
		a.postpone(stack.Numbers(), "the target branch moved")
		return nil
	} else if err != nil {
		return err
	}
	a.options.Logf("fast-forwarded %s to %s", a.config.Target, shortSHA(top.SHA))
	return a.landed(ctx, stack)
}

func (a *applier) landed(ctx context.Context, stack Stack) error {
	for _, number := range stack.Numbers() {
		if err := a.comment(ctx, number, landedComment(stack)); err != nil {
			return err
		}
	}
	a.report.Landed = append(a.report.Landed, stack.Numbers())
	return nil
}

func (a *applier) postpone(prs []int, reason string) {
	a.options.Logf("deferred %s: %s", refs(prs), reason)
	a.report.Deferred = append(a.report.Deferred, prs)
}

// settle cancels, for SettleWait, the pull_request runs the push started on
// every member but the top. Later ticks catch what appears after.
func (a *applier) settle(ctx context.Context, stack Stack) {
	if a.options.SettleWait <= 0 || len(stack.Members) < 2 {
		return
	}
	deadline := a.options.Now().Add(a.options.SettleWait)
	for {
		if ids, err := batchmateRuns(ctx, a.options.GitHub, stack); err == nil {
			a.cancel(ctx, ids)
		}
		if !a.options.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// cancel is best effort: a run that finished meanwhile cannot be cancelled,
// and a missed one only costs runner time.
func (a *applier) cancel(ctx context.Context, ids []int64) {
	for _, id := range ids {
		if err := a.options.GitHub.CancelRun(ctx, id); err != nil {
			a.options.Logf("could not cancel run %d: %v", id, err)
		}
	}
}

func (a *applier) cancelPullRequestRuns(ctx context.Context, sha string) {
	runs, err := a.options.GitHub.WorkflowRuns(ctx, sha)
	if err != nil {
		a.options.Logf("could not list the runs of %s: %v", shortSHA(sha), err)
		return
	}
	var ids []int64
	for _, run := range runs {
		if run.Event == "pull_request" && activeRunStatuses[run.Status] {
			ids = append(ids, run.ID)
		}
	}
	a.cancel(ctx, ids)
}

// keepOwns returns the record, other than current, still worth keeping.
func keepOwns(owns []Own, current Own) []Own {
	for _, own := range owns {
		if own != current {
			return []Own{own}
		}
	}
	return nil
}
