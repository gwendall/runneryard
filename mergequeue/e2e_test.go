package mergequeue

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// e2e drives whole ticks through the real RESTClient and LocalGit against
// fakeGitHub and a bare repository.
type e2e struct {
	t    *testing.T
	bare string
	base string
	gh   *fakeGitHub
	now  time.Time
}

func newE2E(t *testing.T) *e2e {
	bare, base := newBareRepo(t)
	return &e2e{t: t, bare: bare, base: base, gh: newFakeGitHub(t, bare), now: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)}
}

func (e *e2e) options() Options {
	return Options{
		GitHub: e.gh.client(appToken),
		OpenGit: func(ctx context.Context, target string) (Git, error) {
			git, err := NewLocalGit(ctx, filepath.Join(e.t.TempDir(), "clone"), e.bare, target, "")
			if err != nil {
				return nil, err
			}
			return git, nil
		},
		BotLogin: botLogin,
		Now:      func() time.Time { return e.now },
	}
}

func (e *e2e) tick() (Plan, Report) {
	e.t.Helper()
	plan, report, err := Tick(context.Background(), e.options())
	if err != nil {
		e.t.Fatalf("tick: %v", err)
	}
	e.now = e.now.Add(time.Minute)
	return plan, report
}

// open pushes a branch with files on top of the current main and queues it
// as a pull request labeled by alice, who has write access.
func (e *e2e) open(number int, files map[string]string) {
	branch := "feature-" + string(rune('0'+number))
	pushBranch(e.t, e.bare, branch, e.base, files)
	e.gh.open(number, branch, "alice", e.now.Add(time.Duration(number)*time.Second))
}

func (e *e2e) head(number int) string { return e.gh.ref(e.gh.prs[number].branch) }

func (e *e2e) files(sha string) []string {
	return strings.Fields(runGit(e.t, e.bare, "ls-tree", "-r", "--name-only", sha))
}

func wantReport(t *testing.T, got, want [][]int, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func TestEndToEndLandsAStackAndGitHubMarksItMerged(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.open(2, map[string]string{"two.txt": "2\n"})
	e.open(3, map[string]string{"three.txt": "3\n"})

	_, report := e.tick()
	wantReport(t, report.Formed, [][]int{{1, 2}}, "formed")
	top := e.head(2)
	if parent := runGit(t, e.bare, "rev-parse", top+"^"); parent != e.head(1) {
		t.Fatalf("#2 is not stacked on #1: parent %s, #1 head %s", parent, e.head(1))
	}
	if !strings.Contains(e.gh.status(2), "runneryard-merge-queue:stack base="+e.base) {
		t.Fatalf("#2 status lacks the stack marker:\n%s", e.gh.status(2))
	}
	if !strings.Contains(e.gh.status(1), "runneryard-merge-queue:own head="+e.head(1)+" upstream="+e.base) {
		t.Fatalf("#1 status lacks its own-commits marker:\n%s", e.gh.status(1))
	}
	if !strings.Contains(e.gh.status(3), "position 1 of 1") {
		t.Fatalf("#3 status = %q, want a waiting position", e.gh.status(3))
	}

	// The push started CI on #1 too; the top run already covers it.
	batchmate := e.gh.ci(e.head(1), defaultCheck, "", e.now)
	plan, report := e.tick()
	if !plan.Idle || len(report.Formed) != 0 {
		t.Fatalf("pending tick did more than wait: %+v", plan)
	}
	if !slices.Contains(e.gh.cancelled, batchmate) {
		t.Fatalf("batchmate run %d not cancelled (cancelled %v)", batchmate, e.gh.cancelled)
	}

	e.gh.ci(top, defaultCheck, "success", e.now)
	_, report = e.tick()
	wantReport(t, report.Landed, [][]int{{1, 2}}, "landed")
	if got := e.gh.ref("main"); got != top {
		t.Fatalf("main = %s, want the tested top %s", got, top)
	}
	if !e.gh.isMerged(1) || !e.gh.isMerged(2) {
		t.Fatal("GitHub must see both pull requests merged once main contains their heads")
	}
	if !strings.Contains(e.gh.status(1), "landed on the target as "+shortSHA(top)) {
		t.Fatalf("#1 status = %q", e.gh.status(1))
	}

	_, report = e.tick()
	wantReport(t, report.Formed, [][]int{{3}}, "formed after landing")
	if parent := runGit(t, e.bare, "rev-parse", e.head(3)+"^"); parent != top {
		t.Fatalf("#3 was not rebased onto the new main")
	}
}

func TestEndToEndRetriesOnceThenSplitsWithoutCarryingBatchmates(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.open(2, map[string]string{"two.txt": "2\n"})
	e.tick()
	top := e.head(2)

	failed := e.gh.ci(top, defaultCheck, "failure", e.now.Add(-time.Minute))
	_, report := e.tick()
	wantReport(t, report.Retried, [][]int{{1, 2}}, "retried")
	if !reflect.DeepEqual(e.gh.reruns, []int64{failed}) {
		t.Fatalf("reruns = %v, want [%d]", e.gh.reruns, failed)
	}
	if !strings.Contains(e.gh.status(2), "retry=1") {
		t.Fatalf("#2 marker not marked retried:\n%s", e.gh.status(2))
	}

	// Until the re-run reports, the old failure does not count.
	if plan, _ := e.tick(); !plan.Idle {
		t.Fatalf("tick before the re-run reported acted: %+v", plan.Actions)
	}

	e.gh.ci(top, defaultCheck, "failure", e.now)
	_, report = e.tick()
	wantReport(t, report.Split, [][]int{{1, 2}}, "split")
	wantReport(t, report.Formed, [][]int{{1}, {2}}, "formed halves")
	if slices.Contains(e.files(e.head(2)), "one.txt") {
		t.Fatal("#2 alone still carries #1's commit: its branch was rebuilt from the stacked branch, not from its own commits")
	}
	if parent := runGit(t, e.bare, "rev-parse", e.head(2)+"^"); parent != e.base {
		t.Fatalf("#2 alone is not rebased onto main")
	}

	// #2 fails a check no workflow run can re-run: rejected at once.
	e.gh.mu.Lock()
	conclusion := "failure"
	e.gh.checkRuns[e.head(2)] = append(e.gh.checkRuns[e.head(2)], fakeCheckRun{ID: 999, Name: defaultCheck, Status: "completed", Conclusion: &conclusion, HTMLURL: "https://github.example/external/999"})
	e.gh.mu.Unlock()
	e.gh.ci(e.head(1), defaultCheck, "success", e.now)
	_, report = e.tick()
	wantReport(t, report.Landed, [][]int{{1}}, "landed")
	wantReport(t, report.Abandoned, [][]int{{2}}, "abandoned")
	if !slices.Contains(e.files(e.gh.ref("main")), "one.txt") || slices.Contains(e.files(e.gh.ref("main")), "two.txt") {
		t.Fatalf("main files = %v, want one.txt only", e.files(e.gh.ref("main")))
	}

	_, report = e.tick()
	wantReport(t, report.Formed, [][]int{{2}}, "re-formed")
	e.gh.mu.Lock()
	e.gh.checkRuns[e.head(2)] = append(e.gh.checkRuns[e.head(2)], fakeCheckRun{ID: 1000, Name: defaultCheck, Status: "completed", Conclusion: &conclusion, HTMLURL: "https://github.example/external/1000"})
	e.gh.mu.Unlock()
	_, report = e.tick()
	if !reflect.DeepEqual(report.Rejected, []int{2}) || e.gh.hasLabel(2) {
		t.Fatalf("rejected = %v, label kept = %v; want #2 rejected and unlabeled", report.Rejected, e.gh.hasLabel(2))
	}
	if status := e.gh.status(2); !strings.Contains(status, "rejected (required checks failed: ci)") || !strings.Contains(status, "https://github.example/external/1000") {
		t.Fatalf("#2 status = %q", status)
	}
}

func TestEndToEndConflictsSplitThenReject(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"shared.txt": "from 1\n"})
	e.open(2, map[string]string{"shared.txt": "from 2\n"})

	_, report := e.tick()
	wantReport(t, report.Split, [][]int{{1, 2}}, "split")
	wantReport(t, report.Formed, [][]int{{1}, {2}}, "formed")

	e.gh.ci(e.head(1), defaultCheck, "success", e.now)
	e.tick()
	_, report = e.tick()
	if !reflect.DeepEqual(report.Rejected, []int{2}) {
		t.Fatalf("rejected = %v, want [2]: #2 cannot rebase onto main with #1", report.Rejected)
	}
	if !strings.Contains(e.gh.status(2), "does not rebase cleanly") || e.gh.hasLabel(2) {
		t.Fatalf("#2 status = %q, label kept = %v", e.gh.status(2), e.gh.hasLabel(2))
	}
}

func TestEndToEndAbandonsAStackAMemberLeft(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.open(2, map[string]string{"two.txt": "2\n"})
	e.tick()
	topRun := e.gh.ci(e.head(2), defaultCheck, "", e.now)

	e.gh.unlabel(1)
	_, report := e.tick()
	wantReport(t, report.Abandoned, [][]int{{1, 2}}, "abandoned")
	if !slices.Contains(e.gh.cancelled, topRun) {
		t.Fatal("the abandoned stack's CI kept running")
	}
	if !strings.Contains(e.gh.status(2), "back in the queue (#1 left the queue)") {
		t.Fatalf("#2 status = %q", e.gh.status(2))
	}

	_, report = e.tick()
	wantReport(t, report.Formed, [][]int{{2}}, "re-formed")
	if files := e.files(e.head(2)); slices.Contains(files, "one.txt") {
		t.Fatalf("#2 still carries #1's commit after #1 left the queue: %v", files)
	}
}

func TestEndToEndAbandonsAStackPushedToAndKeepsTheNewCommit(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.open(2, map[string]string{"two.txt": "2\n"})
	e.tick()

	// The author of #2 pulls the rebased branch and adds a fix on top.
	pushBranch(t, e.bare, "feature-2", e.head(2), map[string]string{"fix.txt": "fix\n"})
	_, report := e.tick()
	wantReport(t, report.Abandoned, [][]int{{1, 2}}, "abandoned")

	_, report = e.tick()
	wantReport(t, report.Formed, [][]int{{1, 2}}, "re-formed")
	if files := e.files(e.head(2)); !slices.Contains(files, "fix.txt") || !slices.Contains(files, "one.txt") {
		t.Fatalf("#2 files = %v, want the fix on top of the stack", files)
	}
	// #1 now sits below #2 again: exactly one copy of its commit.
	if count := runGit(t, e.bare, "rev-list", "--count", e.base+".."+e.head(2)); count != "3" {
		t.Fatalf("stack holds %s commits above main, want 3 (#1, #2, fix)", count)
	}
}

func TestEndToEndTrustsOnlyTheQueueBotsMarkers(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"one.txt": "1\n"})
	head := e.head(1)
	// Someone forges a green one-member stack on the unrebased head, and a
	// landed marker, hoping the queue pushes it or skips it.
	forged := StatusPrefix + "\n" + stackMarker(Stack{Base: e.base, Members: []Member{{Number: 1, SHA: head}}, At: e.now}) + "\n" + landedMarker(Landed{Head: head})
	e.gh.addComment(1, fakeUser{Login: humanLogin, Type: "User"}, forged)
	e.gh.addComment(1, fakeUser{Login: "other-app[bot]", Type: "Bot"}, forged)
	e.gh.ci(head, defaultCheck, "success", e.now)

	_, report := e.tick()
	if got := e.gh.ref("main"); got != e.base {
		t.Fatalf("main moved to %s on a forged marker", got)
	}
	wantReport(t, report.Formed, [][]int{{1}}, "formed")
}

func TestEndToEndChecksEligibility(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.gh.prs[1].events[0].Actor["login"] = "tracy" // read access only
	e.open(2, map[string]string{"two.txt": "2\n"})
	e.gh.prs[2].base = "release"
	e.open(3, map[string]string{"three.txt": "3\n"})
	e.gh.prs[3].fork = true
	e.open(4, map[string]string{"four.txt": "4\n"})
	e.gh.prs[4].draft = true
	e.open(5, map[string]string{"five.txt": "5\n"})

	_, report := e.tick()
	wantReport(t, report.Formed, [][]int{{5}}, "formed")
	for number, want := range map[int]string{
		1: "must be added by someone with write access",
		2: "it targets `release`",
		3: "not forks",
		4: "it is a draft",
	} {
		if status := e.gh.status(number); !strings.Contains(status, want) {
			t.Fatalf("#%d status = %q, want %q", number, status, want)
		}
	}

	// An admin labels #1 again: the latest labeler counts.
	e.gh.label(1, "root", e.now)
	e.gh.ci(e.head(5), defaultCheck, "success", e.now)
	e.tick()
	_, report = e.tick()
	wantReport(t, report.Formed, [][]int{{1}}, "formed after an admin labeled")
}

func TestEndToEndLeavesLandedPullRequestsAloneUntilGitHubCatchesUp(t *testing.T) {
	e := newE2E(t)
	e.gh.mergeLag = true
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.tick()
	e.gh.ci(e.head(1), defaultCheck, "success", e.now)
	_, report := e.tick()
	wantReport(t, report.Landed, [][]int{{1}}, "landed")

	plan, report := e.tick()
	if !plan.Idle || len(report.Formed) != 0 || len(plan.Actions) != 0 {
		t.Fatalf("tick during merge lag acted on a landed pull request: %v", plan.Actions)
	}
}

// Every member of a landed stack records its own head, not the top's: a
// lower member still open during the merge lag must not be stacked again.
func TestEndToEndLeavesEveryMemberOfALandedStackAloneUntilGitHubCatchesUp(t *testing.T) {
	e := newE2E(t)
	e.gh.mergeLag = true
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.open(2, map[string]string{"two.txt": "2\n"})
	e.tick()
	e.gh.ci(e.head(2), defaultCheck, "success", e.now)
	_, report := e.tick()
	wantReport(t, report.Landed, [][]int{{1, 2}}, "landed")
	if status := e.gh.status(1); !strings.Contains(status, "together with #2.") || strings.Contains(status, "#1") {
		t.Fatalf("#1 status = %q, want it to name only its batchmate", status)
	}

	plan, report := e.tick()
	if !plan.Idle || len(report.Formed) != 0 || len(plan.Actions) != 0 {
		t.Fatalf("tick during merge lag acted on a landed pull request: %v", plan.Actions)
	}
}

func TestEndToEndRecordsAStackTheTargetAlreadyContains(t *testing.T) {
	e := newE2E(t)
	e.gh.mergeLag = true
	e.open(1, map[string]string{"one.txt": "1\n"})
	e.tick()
	// A tick fast-forwarded main, then died before writing its comments.
	runGit(t, e.bare, "update-ref", "refs/heads/main", e.head(1))
	e.gh.ci(e.head(1), defaultCheck, "success", e.now)

	_, report := e.tick()
	wantReport(t, report.Landed, [][]int{{1}}, "landed")
	if len(report.Abandoned) != 0 || len(report.Formed) != 0 {
		t.Fatalf("report = %+v, want only the landing recorded", report)
	}
}

func TestEndToEndDryRunWritesNothing(t *testing.T) {
	e := newE2E(t)
	e.open(1, map[string]string{"one.txt": "1\n"})
	plan, _, err := Tick(context.Background(), Options{GitHub: e.gh.client(humanToken), DryRun: true, Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionForm {
		t.Fatalf("plan = %v, want one form", plan.Actions)
	}
	if writes := e.gh.writeLog(); len(writes) != 0 {
		t.Fatalf("dry run wrote: %v", writes)
	}

	if _, _, err := Tick(context.Background(), Options{GitHub: e.gh.client(appToken)}); err == nil {
		t.Fatal("a tick that writes ran without the bot login")
	}
}
