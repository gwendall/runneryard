package mergequeue

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// activeRunStatuses are the workflow run statuses that can still be
// cancelled.
var activeRunStatuses = map[string]bool{"queued": true, "in_progress": true, "waiting": true, "requested": true, "pending": true}

// rerunnableConclusions are the workflow run conclusions GitHub can re-run
// the failed jobs of.
var rerunnableConclusions = map[string]bool{"failure": true, "timed_out": true, "cancelled": true}

// LoadSnapshot reads everything a tick needs from GitHub. Without botLogin
// it cannot tell the queue's own status comments from anyone else's, so it
// trusts none of them and sees no stack in flight.
func LoadSnapshot(ctx context.Context, github GitHub, config Config, botLogin string, now time.Time) (Snapshot, error) {
	targetSHA, err := github.ResolveBranch(ctx, config.Target)
	if err != nil {
		return Snapshot{}, err
	}
	prs, err := github.QueuedPRs(ctx, config.Label)
	if err != nil {
		return Snapshot{}, err
	}
	sortFIFO(prs)

	snapshot := Snapshot{Now: now, TargetSHA: targetSHA}
	canWrite := map[string]bool{}
	var stacks []Stack
	for _, pr := range prs {
		queued := QueuedPR{PR: pr}
		if pr.LabeledBy != "" {
			allowed, known := canWrite[pr.LabeledBy]
			if !known {
				if allowed, err = github.CanWrite(ctx, pr.LabeledBy); err != nil {
					return Snapshot{}, err
				}
				canWrite[pr.LabeledBy] = allowed
			}
			queued.LabelerCanWrite = allowed
		}
		if botLogin != "" {
			body, err := github.StatusComment(ctx, pr.Number, botLogin)
			if err != nil {
				return Snapshot{}, err
			}
			if landed, ok := ParseLanded(body); ok && landed.Head == pr.HeadSHA {
				queued.Landed = true
			}
			queued.Owns = ParseOwns(body)
			// The stack marker is trusted only on its own top pull request.
			if stack, ok := ParseStack(body); ok && stack.Top().Number == pr.Number {
				stacks = append(stacks, stack)
			}
		}
		snapshot.Queued = append(snapshot.Queued, queued)
	}

	queued := make(map[int]QueuedPR, len(snapshot.Queued))
	for _, pr := range snapshot.Queued {
		queued[pr.Number] = pr
	}
	for _, stack := range stacks {
		inFlight, err := loadInFlight(ctx, github, config, stack, targetSHA, queued, now)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Stacks = append(snapshot.Stacks, inFlight)
	}
	return snapshot, nil
}

func loadInFlight(ctx context.Context, github GitHub, config Config, stack Stack, targetSHA string, queued map[int]QueuedPR, now time.Time) (InFlight, error) {
	inFlight := InFlight{Stack: stack}
	top := stack.Top()
	if stack.Base != targetSHA {
		landed := top.SHA == targetSHA
		if !landed {
			var err error
			if landed, err = github.IsAncestor(ctx, top.SHA, targetSHA); err != nil {
				return InFlight{}, err
			}
		}
		inFlight.Landed = landed
		return inFlight, nil
	}
	if staleReason(stack, targetSHA, queued) != "" {
		// The planner abandons it; its checks do not matter.
		return inFlight, nil
	}

	var files []string
	if needsFiles(config.RequiredChecks) {
		for _, number := range stack.Numbers() {
			changed, err := github.ChangedFiles(ctx, number)
			if err != nil {
				return InFlight{}, err
			}
			files = append(files, changed...)
		}
	}
	runs, err := github.CheckRuns(ctx, top.SHA)
	if err != nil {
		return InFlight{}, err
	}
	var retriedAt time.Time
	if stack.Retry > 0 {
		retriedAt = stack.At
	}
	inFlight.Checks = Evaluate(config.RequiredChecks, files, runs, now.Sub(stack.At), config.CheckTimeout, retriedAt)

	switch inFlight.Checks.State {
	case StateFailure:
		if stack.Retry > 0 || len(inFlight.Checks.Suites) == 0 {
			return inFlight, nil
		}
		workflowRuns, err := github.WorkflowRuns(ctx, top.SHA)
		if err != nil {
			return InFlight{}, err
		}
		for _, run := range workflowRuns {
			if run.Status == "completed" && rerunnableConclusions[run.Conclusion] && slices.Contains(inFlight.Checks.Suites, run.SuiteID) {
				inFlight.RerunIDs = append(inFlight.RerunIDs, run.ID)
			}
		}
	case StatePending:
		batchmateRuns, err := batchmateRuns(ctx, github, stack)
		if err != nil {
			return InFlight{}, err
		}
		inFlight.BatchmateRuns = batchmateRuns
	}
	return inFlight, nil
}

// batchmateRuns lists the pull_request runs still queued or running on the
// commits of every member but the top: the top commit contains their code,
// so its own run is the one that counts.
func batchmateRuns(ctx context.Context, github GitHub, stack Stack) ([]int64, error) {
	var ids []int64
	for _, member := range stack.Members[:len(stack.Members)-1] {
		runs, err := github.WorkflowRuns(ctx, member.SHA)
		if err != nil {
			return nil, fmt.Errorf("runs of #%d: %w", member.Number, err)
		}
		for _, run := range runs {
			if run.Event == "pull_request" && activeRunStatuses[run.Status] {
				ids = append(ids, run.ID)
			}
		}
	}
	return ids, nil
}

func needsFiles(checks []RequiredCheck) bool {
	for _, check := range checks {
		if len(check.Paths) > 0 {
			return true
		}
	}
	return false
}

// sortFIFO orders pull requests by the time the label was last added, then
// by number. A pull request with no labeled event goes last.
func sortFIFO(prs []PR) {
	slices.SortStableFunc(prs, func(a, b PR) int {
		switch {
		case a.LabeledAt.IsZero() != b.LabeledAt.IsZero():
			if a.LabeledAt.IsZero() {
				return 1
			}
			return -1
		case !a.LabeledAt.Equal(b.LabeledAt):
			return a.LabeledAt.Compare(b.LabeledAt)
		}
		return a.Number - b.Number
	})
}
