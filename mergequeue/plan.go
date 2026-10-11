package mergequeue

import (
	"fmt"
	"strings"
	"time"
)

// QueuedPR is a queued pull request with what the snapshot learned about it.
type QueuedPR struct {
	PR
	// LabelerCanWrite is true when whoever added the queue label has write,
	// maintain or admin access. Triage access can add labels, so the queue
	// does not trust the label alone.
	LabelerCanWrite bool
	// Owns are the pull request's own commit ranges recorded by earlier
	// stacks that rewrote its branch, newest first.
	Owns []Own
	// Landed is true when the queue already landed this head and GitHub has
	// not marked the pull request merged yet. A tick leaves it alone.
	Landed bool
}

// InFlight is a stack found through the marker on its top pull request.
type InFlight struct {
	Stack Stack
	// Landed is true when the target already contains the top commit.
	Landed bool
	Checks Status
	// RerunIDs are the completed workflow runs on the top commit that hold
	// a failing required check.
	RerunIDs []int64
	// BatchmateRuns are queued or running pull_request workflow runs on the
	// commits of the other members: the top run already tests their code.
	BatchmateRuns []int64
}

// Snapshot is everything a tick knows, read from GitHub before planning.
type Snapshot struct {
	Now       time.Time
	TargetSHA string
	// Queued lists every open pull request carrying the label, FIFO by the
	// time the label was last added, then by number.
	Queued []QueuedPR
	Stacks []InFlight
}

// ActionKind names one step of a Plan.
type ActionKind string

// Plan steps.
const (
	// ActionForm rebases PRs on top of each other above Base and pushes
	// every branch atomically.
	ActionForm ActionKind = "form"
	// ActionAdvance fast-forwards the target to the stack's top commit.
	ActionAdvance ActionKind = "advance"
	// ActionLanded records a stack the target already contains.
	ActionLanded ActionKind = "landed"
	// ActionRetry re-runs the failed jobs of the stack's top commit once.
	ActionRetry ActionKind = "retry"
	// ActionReject removes the label from one pull request.
	ActionReject ActionKind = "reject"
	// ActionSplit records a failed stack about to be bisected.
	ActionSplit ActionKind = "split"
	// ActionAbandon drops a stack that no longer matches GitHub.
	ActionAbandon ActionKind = "abandon"
	// ActionCancel cancels the batchmate runs of a stack in flight.
	ActionCancel ActionKind = "cancel"
	// ActionComment updates one status comment.
	ActionComment ActionKind = "comment"
)

// Action is one step of a Plan. Only the fields its Kind uses are set.
type Action struct {
	Kind   ActionKind
	Stack  Stack
	PRs    []int
	Base   string
	Number int
	Reason string
	URLs   []string
	RunIDs []int64
	Body   string
}

// String renders the action for the plan printed by a dry run.
func (a Action) String() string {
	switch a.Kind {
	case ActionForm:
		return fmt.Sprintf("form a stack of %s on %s", refs(a.PRs), shortSHA(a.Base))
	case ActionAdvance:
		return fmt.Sprintf("fast-forward the target to %s, landing %s", shortSHA(a.Stack.Top().SHA), refs(a.Stack.Numbers()))
	case ActionLanded:
		return fmt.Sprintf("record %s as landed", refs(a.Stack.Numbers()))
	case ActionRetry:
		return fmt.Sprintf("re-run %d failed workflow run(s) of the stack %s (%s)", len(a.RunIDs), refs(a.Stack.Numbers()), a.Reason)
	case ActionReject:
		return fmt.Sprintf("reject #%d (%s)", a.Number, a.Reason)
	case ActionSplit:
		return fmt.Sprintf("split the stack %s (%s)", refs(a.Stack.Numbers()), a.Reason)
	case ActionAbandon:
		return fmt.Sprintf("abandon the stack %s (%s)", refs(a.Stack.Numbers()), a.Reason)
	case ActionCancel:
		return fmt.Sprintf("cancel %d batchmate run(s) of the stack %s", len(a.RunIDs), refs(a.Stack.Numbers()))
	case ActionComment:
		text := strings.TrimPrefix(strings.SplitN(a.Body, "\n", 3)[1], "Merge queue: ")
		return fmt.Sprintf("comment on #%d: %s", a.Number, text)
	}
	return string(a.Kind)
}

// Plan is what one tick decided.
type Plan struct {
	Actions []Action
	// Idle is true when the tick changes nothing but status comments.
	Idle bool
}

// PlanTick decides one tick from a snapshot. It does no I/O.
//
// Stacks in flight resolve first: a stack the target already contains is
// recorded, a stack GitHub no longer matches is abandoned, a green stack
// lands, a red one retries once, then splits or is rejected. A new stack
// forms only in a tick where nothing resolved and nothing is in flight, so
// the target moves at most once per tick and every stack is tested against
// the target it will land on.
func PlanTick(snapshot Snapshot, config Config) Plan {
	queued := make(map[int]QueuedPR, len(snapshot.Queued))
	for _, pr := range snapshot.Queued {
		queued[pr.Number] = pr
	}
	stale := make([]string, len(snapshot.Stacks))
	advancing := -1
	for i, inFlight := range snapshot.Stacks {
		if !inFlight.Landed {
			stale[i] = staleReason(inFlight.Stack, snapshot.TargetSHA, queued)
		}
		if advancing < 0 && !inFlight.Landed && stale[i] == "" && inFlight.Checks.State == StateSuccess {
			advancing = i
		}
	}
	// The target moves at most once per tick. Once one stack lands, every
	// other stack was tested against a base that is about to be replaced.
	if advancing >= 0 {
		for i, inFlight := range snapshot.Stacks {
			if i != advancing && !inFlight.Landed && stale[i] == "" {
				stale[i] = fmt.Sprintf("the stack of %s lands first", refs(snapshot.Stacks[advancing].Stack.Numbers()))
			}
		}
	}
	claimed := map[int]int{}
	for i, inFlight := range snapshot.Stacks {
		if inFlight.Landed || stale[i] == "" {
			for _, number := range inFlight.Stack.Numbers() {
				claimed[number]++
			}
		}
	}

	var actions []Action
	resolved := false
	var pending []InFlight
	handled := map[int]bool{}
	for i, inFlight := range snapshot.Stacks {
		stack := inFlight.Stack
		numbers := stack.Numbers()
		if inFlight.Landed {
			actions = append(actions, Action{Kind: ActionLanded, Stack: stack})
		} else if stale[i] != "" {
			actions = append(actions, Action{Kind: ActionAbandon, Stack: stack, Reason: stale[i]})
		} else {
			switch inFlight.Checks.State {
			case StatePending:
				pending = append(pending, inFlight)
				if len(inFlight.BatchmateRuns) > 0 {
					actions = append(actions, Action{Kind: ActionCancel, Stack: stack, RunIDs: inFlight.BatchmateRuns})
				}
				continue
			case StateSuccess:
				actions = append(actions, Action{Kind: ActionAdvance, Stack: stack})
			default:
				actions = append(actions, failureActions(snapshot, inFlight)...)
			}
		}
		resolved = true
		for _, number := range numbers {
			handled[number] = true
		}
	}
	// A member claimed by a live stack keeps its comment even if an older,
	// stale stack also lists it.
	for i := range actions {
		if actions[i].Kind == ActionAbandon {
			actions[i].PRs = unclaimedMembers(actions[i].Stack, claimed)
		}
	}

	active := map[int]bool{}
	for _, inFlight := range pending {
		for _, number := range inFlight.Stack.Numbers() {
			active[number] = true
		}
	}
	var candidates []QueuedPR
	for _, pr := range snapshot.Queued {
		if pr.Landed || active[pr.Number] || handled[pr.Number] {
			continue
		}
		if reason := ineligibleReason(pr, config); reason != "" {
			actions = append(actions, Action{Kind: ActionComment, Number: pr.Number, Body: ineligibleComment(reason, config.Label, pr.Owns)})
			continue
		}
		candidates = append(candidates, pr)
	}
	var next []int
	if !resolved && len(pending) == 0 {
		for _, pr := range candidates {
			if len(next) == config.BatchSize {
				break
			}
			next = append(next, pr.Number)
		}
	}
	waiting := candidates[len(next):]
	for i, pr := range waiting {
		actions = append(actions, Action{Kind: ActionComment, Number: pr.Number, Body: waitingComment(i+1, len(waiting), pr.Owns)})
	}
	if len(next) > 0 {
		actions = append(actions, Action{Kind: ActionForm, PRs: next, Base: snapshot.TargetSHA})
	}
	return Plan{Actions: actions, Idle: !resolved && len(next) == 0}
}

// failureActions retries a red stack once when its failed jobs can be
// re-run, then rejects a single pull request or splits a stack in two. Both
// halves are rebuilt on the same base and tested in parallel: when the
// culprit is in the front half, the back half still lands.
func failureActions(snapshot Snapshot, inFlight InFlight) []Action {
	stack := inFlight.Stack
	reason := "required checks failed: " + strings.Join(inFlight.Checks.Failing, ", ")
	if stack.Retry == 0 && len(inFlight.RerunIDs) > 0 {
		retried := stack
		retried.Retry = 1
		// The marker keeps whole seconds, like GitHub's started_at.
		retried.At = snapshot.Now.Truncate(time.Second)
		return []Action{{Kind: ActionRetry, Stack: retried, RunIDs: inFlight.RerunIDs, Reason: reason}}
	}
	if len(stack.Members) == 1 {
		return []Action{{Kind: ActionReject, Stack: stack, Number: stack.Members[0].Number, Reason: reason, URLs: inFlight.Checks.URLs}}
	}
	front, back := Split(stack.Numbers())
	return []Action{
		{Kind: ActionSplit, Stack: stack, Reason: reason},
		{Kind: ActionForm, PRs: front, Base: stack.Base},
		{Kind: ActionForm, PRs: back, Base: stack.Base},
	}
}

// staleReason says why a stack no longer matches GitHub, or "" when it
// still does. Membership is checked against every queued pull request, not
// only eligible ones: eligibility gates entry into a new stack only.
func staleReason(stack Stack, targetSHA string, queued map[int]QueuedPR) string {
	if stack.Base != targetSHA {
		return "the target branch moved while the stack was tested"
	}
	for _, member := range stack.Members {
		pr, ok := queued[member.Number]
		if !ok {
			return fmt.Sprintf("#%d left the queue", member.Number)
		}
		if pr.HeadSHA != member.SHA {
			return fmt.Sprintf("#%d was pushed to while the stack was tested", member.Number)
		}
	}
	return ""
}

func unclaimedMembers(stack Stack, claimed map[int]int) []int {
	var numbers []int
	for _, number := range stack.Numbers() {
		if claimed[number] == 0 {
			numbers = append(numbers, number)
		}
	}
	return numbers
}

// ineligibleReason says why a pull request cannot join a new stack, or ""
// when it can.
func ineligibleReason(pr QueuedPR, config Config) string {
	switch {
	case !pr.SameRepo:
		return "the queue only lands branches of this repository, not forks"
	case pr.Draft:
		return "it is a draft"
	case pr.BaseRef != config.Target:
		return fmt.Sprintf("it targets `%s`, and the queue lands on `%s`", pr.BaseRef, config.Target)
	case !pr.LabelerCanWrite:
		return fmt.Sprintf("the `%s` label must be added by someone with write access", config.Label)
	}
	return ""
}
