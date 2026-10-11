package mergequeue

import (
	"testing"
	"time"
)

func TestApplicableMatchesTrailingGlobstarAndPlainGlob(t *testing.T) {
	check := RequiredCheck{Name: "api", Paths: []string{"services/api/**", "shared/*.go"}}
	for _, test := range []struct {
		file string
		want bool
	}{
		{"services/api/main.go", true},
		{"services/api/sub/dir/file.go", true},
		{"services/api", true},
		{"services/web/main.go", false},
		{"shared/types.go", true},
		{"shared/nested/types.go", false},
	} {
		if got := Applicable(check, []string{test.file}); got != test.want {
			t.Fatalf("Applicable(%v) = %v, want %v", test.file, got, test.want)
		}
	}
	if !Applicable(RequiredCheck{Name: "always"}, nil) {
		t.Fatal("a check with no paths must always be applicable")
	}
}

func TestEvaluate(t *testing.T) {
	required := []RequiredCheck{
		{Name: "unit"},
		{Name: "frontend", Paths: []string{"apps/web/**"}},
	}
	timeout := 45 * time.Minute

	t.Run("all applicable checks green is success", func(t *testing.T) {
		runs := []CheckRun{
			{Name: "unit", Status: "completed", Conclusion: "success"},
			{Name: "frontend", Status: "completed", Conclusion: "success"},
		}
		status := Evaluate(required, []string{"apps/web/index.ts"}, runs, time.Minute, timeout, time.Time{})
		if status.State != StateSuccess {
			t.Fatalf("status = %#v, want success", status)
		}
	})

	t.Run("inapplicable check is not required even absent", func(t *testing.T) {
		runs := []CheckRun{{Name: "unit", Status: "completed", Conclusion: "success"}}
		status := Evaluate(required, []string{"docs/readme.md"}, runs, time.Minute, timeout, time.Time{})
		if status.State != StateSuccess {
			t.Fatalf("status = %#v, want success (frontend check must not be required)", status)
		}
	})

	t.Run("missing applicable check is pending before the timeout", func(t *testing.T) {
		runs := []CheckRun{{Name: "unit", Status: "completed", Conclusion: "success"}}
		status := Evaluate(required, []string{"apps/web/index.ts"}, runs, time.Minute, timeout, time.Time{})
		if status.State != StatePending {
			t.Fatalf("status = %#v, want pending", status)
		}
	})

	t.Run("missing applicable check fails after the timeout", func(t *testing.T) {
		runs := []CheckRun{{Name: "unit", Status: "completed", Conclusion: "success"}}
		status := Evaluate(required, []string{"apps/web/index.ts"}, runs, timeout, timeout, time.Time{})
		if status.State != StateFailure || len(status.Failing) != 1 || status.Failing[0] != "frontend" {
			t.Fatalf("status = %#v, want failure naming frontend", status)
		}
	})

	t.Run("a failing check wins over a pending one", func(t *testing.T) {
		runs := []CheckRun{
			{Name: "unit", Status: "completed", Conclusion: "failure"},
			{Name: "frontend", Status: "in_progress"},
		}
		status := Evaluate(required, []string{"apps/web/index.ts"}, runs, time.Minute, timeout, time.Time{})
		if status.State != StateFailure || len(status.Failing) != 1 || status.Failing[0] != "unit" {
			t.Fatalf("status = %#v, want failure naming unit", status)
		}
	})

	t.Run("neutral and skipped count as passed", func(t *testing.T) {
		runs := []CheckRun{
			{Name: "unit", Status: "completed", Conclusion: "neutral"},
			{Name: "frontend", Status: "completed", Conclusion: "skipped"},
		}
		status := Evaluate(required, []string{"apps/web/index.ts"}, runs, time.Minute, timeout, time.Time{})
		if status.State != StateSuccess {
			t.Fatalf("status = %#v, want success", status)
		}
	})

	t.Run("the newest run of a name wins", func(t *testing.T) {
		runs := []CheckRun{
			{ID: 9, Name: "unit", Status: "completed", Conclusion: "success"},
			{ID: 3, Name: "unit", Status: "completed", Conclusion: "failure"},
		}
		status := Evaluate(required, nil, runs, time.Minute, timeout, time.Time{})
		if status.State != StateSuccess {
			t.Fatalf("status = %#v, want success from the newer run", status)
		}
	})

	t.Run("a failure reports its link and suite", func(t *testing.T) {
		runs := []CheckRun{{ID: 1, Name: "unit", Status: "completed", Conclusion: "timed_out", URL: "https://example.invalid/run/1", SuiteID: 77}}
		status := Evaluate(required, nil, runs, time.Minute, timeout, time.Time{})
		if status.State != StateFailure || len(status.URLs) != 1 || status.URLs[0] != "https://example.invalid/run/1" || len(status.Suites) != 1 || status.Suites[0] != 77 {
			t.Fatalf("status = %#v, want failure with link and suite", status)
		}
	})

	t.Run("after a retry an older failure waits for its new attempt", func(t *testing.T) {
		retriedAt := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
		old := []CheckRun{{ID: 1, Name: "unit", Status: "completed", Conclusion: "failure", StartedAt: retriedAt.Add(-5 * time.Minute)}}
		if status := Evaluate(required, nil, old, time.Minute, timeout, retriedAt); status.State != StatePending {
			t.Fatalf("status = %#v, want pending until the re-run reports", status)
		}
		if status := Evaluate(required, nil, old, timeout, timeout, retriedAt); status.State != StateFailure {
			t.Fatalf("status = %#v, want failure once the timeout passes", status)
		}
		again := append(old, CheckRun{ID: 2, Name: "unit", Status: "completed", Conclusion: "failure", StartedAt: retriedAt.Add(time.Minute)})
		if status := Evaluate(required, nil, again, time.Minute, timeout, retriedAt); status.State != StateFailure {
			t.Fatalf("status = %#v, want failure from the new attempt", status)
		}
		// A check that passed before the retry was not re-run and still counts.
		passed := []CheckRun{{ID: 1, Name: "unit", Status: "completed", Conclusion: "success", StartedAt: retriedAt.Add(-5 * time.Minute)}}
		if status := Evaluate(required, nil, passed, time.Minute, timeout, retriedAt); status.State != StateSuccess {
			t.Fatalf("status = %#v, want success", status)
		}
	})
}
