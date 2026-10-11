package mergequeue

import (
	"path/filepath"
	"strings"
	"time"
)

// Applicable reports whether check must be satisfied for a stack that
// changed changedFiles. A check with no Paths is always applicable. A
// pattern ending in "/**" matches the directory itself and everything
// under it; any other pattern matches the way filepath.Match does (a
// mid-pattern "**" is not supported, only a trailing one).
func Applicable(check RequiredCheck, changedFiles []string) bool {
	if len(check.Paths) == 0 {
		return true
	}
	for _, file := range changedFiles {
		for _, pattern := range check.Paths {
			if matchPath(pattern, file) {
				return true
			}
		}
	}
	return false
}

func matchPath(pattern, file string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return file == prefix || strings.HasPrefix(file, prefix+"/")
	}
	ok, _ := filepath.Match(pattern, file)
	return ok
}

// Status is the outcome of evaluating a stack's required checks.
type Status struct {
	// State is StatePending, StateSuccess or StateFailure.
	State string
	// Failing names the required, applicable checks that failed or never
	// reported within the timeout. URLs links the failed ones, and Suites
	// holds their check suites, which map to rerunnable workflow runs.
	Failing []string
	URLs    []string
	Suites  []int64
}

// Check states.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
)

// passed lists the conclusions that count as a pass. GitHub reports a job
// skipped by its own `if:` as skipped and an informational check as neutral.
var passed = map[string]bool{"success": true, "neutral": true, "skipped": true}

// Evaluate decides a stack's Status from the check runs GitHub reported on
// its top commit. When a check name has several runs (a re-run), the newest
// one counts.
//
// A required check that does not apply to changedFiles is satisfied
// immediately: a path-filtered workflow GitHub never triggered must count as
// a pass, never as a hang. A required check that never reports fails once
// age reaches timeout. A failure wins over a pending check, so a broken
// stack does not wait out the timeout.
//
// retriedAt is set once the queue re-ran the stack's failed jobs: a failure
// that started before it belongs to the attempt being re-run and counts as
// pending until its new attempt reports.
func Evaluate(required []RequiredCheck, changedFiles []string, runs []CheckRun, age, timeout time.Duration, retriedAt time.Time) Status {
	newest := make(map[string]CheckRun, len(runs))
	for _, run := range runs {
		if current, ok := newest[run.Name]; !ok || run.ID > current.ID {
			newest[run.Name] = run
		}
	}
	pending := false
	var status Status
	for _, check := range required {
		if !Applicable(check, changedFiles) {
			continue
		}
		run, reported := newest[check.Name]
		if reported && run.Status == "completed" && !passed[run.Conclusion] &&
			!retriedAt.IsZero() && run.StartedAt.Before(retriedAt) {
			reported = false
		}
		switch {
		case !reported:
			if age >= timeout {
				status.Failing = append(status.Failing, check.Name)
			} else {
				pending = true
			}
		case run.Status != "completed":
			pending = true
		case !passed[run.Conclusion]:
			status.Failing = append(status.Failing, check.Name)
			if run.URL != "" {
				status.URLs = append(status.URLs, run.URL)
			}
			if run.SuiteID != 0 {
				status.Suites = append(status.Suites, run.SuiteID)
			}
		}
	}
	switch {
	case len(status.Failing) > 0:
		status.State = StateFailure
	case pending:
		status.State = StatePending
	default:
		status.State = StateSuccess
	}
	return status
}
