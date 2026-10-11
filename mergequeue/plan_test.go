package mergequeue

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	planNow  = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	planBase = strings.Repeat("b", 40)
)

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func queuedPR(number int, head string) QueuedPR {
	return QueuedPR{
		PR: PR{
			Number: number, HeadSHA: head, HeadRef: "branch", BaseRef: "main", SameRepo: true,
			LabeledAt: planNow.Add(time.Duration(number) * time.Second), LabeledBy: "alice",
		},
		LabelerCanWrite: true,
	}
}

func kinds(plan Plan) []ActionKind {
	var out []ActionKind
	for _, action := range plan.Actions {
		out = append(out, action.Kind)
	}
	return out
}

func testConfig() Config {
	return Config{RequiredChecks: []RequiredCheck{{Name: "ci"}}, BatchSize: 2}.WithDefaults()
}

func TestPlanTickFormsTheHeadOfTheQueue(t *testing.T) {
	snapshot := Snapshot{Now: planNow, TargetSHA: planBase, Queued: []QueuedPR{
		queuedPR(1, sha('1')), queuedPR(2, sha('2')), queuedPR(3, sha('3')), queuedPR(4, sha('4')),
	}}
	plan := PlanTick(snapshot, testConfig())
	if plan.Idle {
		t.Fatal("a tick that forms a stack is not idle")
	}
	last := plan.Actions[len(plan.Actions)-1]
	if last.Kind != ActionForm || !reflect.DeepEqual(last.PRs, []int{1, 2}) || last.Base != planBase {
		t.Fatalf("last action = %+v, want a form of #1, #2 on the target", last)
	}
	if !strings.Contains(plan.Actions[0].Body, "position 1 of 2") || plan.Actions[0].Number != 3 {
		t.Fatalf("first action = %+v, want #3 at position 1 of 2", plan.Actions[0])
	}
}

func TestPlanTickWaitsForAStackInFlight(t *testing.T) {
	stack := Stack{Base: planBase, Members: []Member{{1, sha('1')}, {2, sha('2')}}, At: planNow}
	snapshot := Snapshot{Now: planNow, TargetSHA: planBase,
		Queued: []QueuedPR{queuedPR(1, sha('1')), queuedPR(2, sha('2')), queuedPR(3, sha('3'))},
		Stacks: []InFlight{{Stack: stack, Checks: Status{State: StatePending}, BatchmateRuns: []int64{7}}},
	}
	plan := PlanTick(snapshot, testConfig())
	if !plan.Idle || !reflect.DeepEqual(kinds(plan), []ActionKind{ActionCancel, ActionComment}) {
		t.Fatalf("plan = %v, want only the batchmate cancel and #3 waiting", plan.Actions)
	}
}

func TestPlanTickRetriesOnlyWhatCanBeRerun(t *testing.T) {
	stack := Stack{Base: planBase, Members: []Member{{1, sha('1')}, {2, sha('2')}}, At: planNow.Add(-time.Hour)}
	queued := []QueuedPR{queuedPR(1, sha('1')), queuedPR(2, sha('2'))}
	failed := Status{State: StateFailure, Failing: []string{"ci"}}

	plan := PlanTick(Snapshot{Now: planNow, TargetSHA: planBase, Queued: queued,
		Stacks: []InFlight{{Stack: stack, Checks: failed, RerunIDs: []int64{42}}}}, testConfig())
	if !reflect.DeepEqual(kinds(plan), []ActionKind{ActionRetry}) {
		t.Fatalf("plan = %v, want one retry", plan.Actions)
	}
	if retried := plan.Actions[0].Stack; retried.Retry != 1 || !retried.At.Equal(planNow) {
		t.Fatalf("retried stack = %+v, want retry=1 at the tick time", retried)
	}

	// No run to re-run (an external check, or a check that never reported):
	// split straight away.
	plan = PlanTick(Snapshot{Now: planNow, TargetSHA: planBase, Queued: queued,
		Stacks: []InFlight{{Stack: stack, Checks: failed}}}, testConfig())
	if !reflect.DeepEqual(kinds(plan), []ActionKind{ActionSplit, ActionForm, ActionForm}) {
		t.Fatalf("plan = %v, want a split into two forms", plan.Actions)
	}
	if !reflect.DeepEqual(plan.Actions[1].PRs, []int{1}) || !reflect.DeepEqual(plan.Actions[2].PRs, []int{2}) {
		t.Fatalf("halves = %v and %v", plan.Actions[1].PRs, plan.Actions[2].PRs)
	}

	single := Stack{Base: planBase, Members: []Member{{1, sha('1')}}, Retry: 1, At: planNow}
	plan = PlanTick(Snapshot{Now: planNow, TargetSHA: planBase, Queued: queued[:1],
		Stacks: []InFlight{{Stack: single, Checks: Status{State: StateFailure, Failing: []string{"ci"}, URLs: []string{"u"}}, RerunIDs: []int64{42}}}}, testConfig())
	if !reflect.DeepEqual(kinds(plan), []ActionKind{ActionReject}) || plan.Actions[0].Number != 1 || plan.Actions[0].URLs[0] != "u" {
		t.Fatalf("plan = %v, want #1 rejected after its retry", plan.Actions)
	}
}

func TestPlanTickMovesTheTargetOncePerTick(t *testing.T) {
	front := Stack{Base: planBase, Members: []Member{{1, sha('1')}}, At: planNow}
	back := Stack{Base: planBase, Members: []Member{{2, sha('2')}}, At: planNow}
	snapshot := Snapshot{Now: planNow, TargetSHA: planBase,
		Queued: []QueuedPR{queuedPR(1, sha('1')), queuedPR(2, sha('2'))},
		Stacks: []InFlight{{Stack: front, Checks: Status{State: StateSuccess}}, {Stack: back, Checks: Status{State: StateSuccess}}},
	}
	plan := PlanTick(snapshot, testConfig())
	if !reflect.DeepEqual(kinds(plan), []ActionKind{ActionAdvance, ActionAbandon}) {
		t.Fatalf("plan = %v, want one advance and the other stack abandoned", plan.Actions)
	}
	if !strings.Contains(plan.Actions[1].Reason, "#1 lands first") {
		t.Fatalf("abandon reason = %q", plan.Actions[1].Reason)
	}
}

func TestPlanTickAbandonsStaleStacksWithoutTouchingLiveOnes(t *testing.T) {
	// An old stack of #1, #2 whose marker survived on #2, while #1 already
	// tops a live stack of its own.
	old := Stack{Base: planBase, Members: []Member{{1, sha('a')}, {2, sha('2')}}, At: planNow}
	live := Stack{Base: planBase, Members: []Member{{1, sha('1')}}, At: planNow}
	snapshot := Snapshot{Now: planNow, TargetSHA: planBase,
		Queued: []QueuedPR{queuedPR(1, sha('1')), queuedPR(2, sha('2'))},
		Stacks: []InFlight{{Stack: live, Checks: Status{State: StatePending}}, {Stack: old}},
	}
	plan := PlanTick(snapshot, testConfig())
	if !reflect.DeepEqual(kinds(plan), []ActionKind{ActionAbandon}) {
		t.Fatalf("plan = %v, want one abandon", plan.Actions)
	}
	if abandon := plan.Actions[0]; !reflect.DeepEqual(abandon.PRs, []int{2}) || !strings.Contains(abandon.Reason, "#1 was pushed to") {
		t.Fatalf("abandon = %+v, want only #2's comment rewritten", abandon)
	}
}

func TestPlanTickSkipsLandedAndIneligiblePullRequests(t *testing.T) {
	landed := queuedPR(1, sha('1'))
	landed.Landed = true
	triaged := queuedPR(2, sha('2'))
	triaged.LabelerCanWrite = false
	other := queuedPR(3, sha('3'))
	other.BaseRef = "release"
	plan := PlanTick(Snapshot{Now: planNow, TargetSHA: planBase, Queued: []QueuedPR{landed, triaged, other}}, testConfig())
	if !plan.Idle || !reflect.DeepEqual(kinds(plan), []ActionKind{ActionComment, ActionComment}) {
		t.Fatalf("plan = %v, want two ineligibility comments", plan.Actions)
	}
	if plan.Actions[0].Number != 2 || plan.Actions[1].Number != 3 {
		t.Fatalf("comments on #%d and #%d, want #2 and #3", plan.Actions[0].Number, plan.Actions[1].Number)
	}
}

func TestSortFIFOUsesLabelTimeThenNumber(t *testing.T) {
	prs := []PR{
		{Number: 5, LabeledAt: planNow.Add(time.Minute)},
		{Number: 9},
		{Number: 3, LabeledAt: planNow.Add(time.Minute)},
		{Number: 7, LabeledAt: planNow},
	}
	sortFIFO(prs)
	var got []int
	for _, pr := range prs {
		got = append(got, pr.Number)
	}
	if !reflect.DeepEqual(got, []int{7, 3, 5, 9}) {
		t.Fatalf("order = %v, want [7 3 5 9]", got)
	}
}
