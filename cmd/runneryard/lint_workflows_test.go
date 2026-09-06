package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorkflow(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findingKinds(report lintReport) map[string]int {
	kinds := map[string]int{}
	for _, f := range report.Findings {
		kinds[f.Kind]++
	}
	return kinds
}

// The shape measured on 2026-09-05, in miniature: two workflows calling the same
// planner, a guard job of one step, a workflow that runs on every pull request, and
// a job with no budget.
func TestLintWorkflowsNamesTheShapesThatCostMachines(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "daemon-ci.yml", `name: Daemon CI
on:
  pull_request:
    paths: ['platform/apps/daemon/**']
jobs:
  lock_impact:
    uses: ./.github/workflows/workspace-impact.yml
    with:
      scopes_json: '["platform/apps/daemon/**"]'
  unit:
    needs: lock_impact
    runs-on: kami-linux-x64
    timeout-minutes: 20
    strategy:
      matrix:
        shard: [1, 2]
    steps:
      - uses: actions/checkout@v4
      - run: bun test --shard ${{ matrix.shard }}
  gate:
    needs: unit
    runs-on: kami-linux-x64
    timeout-minutes: 2
    steps:
      - run: echo green
`)
	writeWorkflow(t, dir, "web-ci.yml", `name: Web CI
on:
  pull_request:
jobs:
  lock_impact:
    uses: ./.github/workflows/workspace-impact.yml
  typecheck:
    runs-on: kami-linux-x64
    steps:
      - uses: actions/checkout@v4
      - run: pnpm install
      - run: pnpm typecheck
`)
	writeWorkflow(t, dir, "workspace-impact.yml", `name: Workspace impact
on:
  workflow_call:
jobs:
  impact:
    runs-on: kami-linux-x64
    timeout-minutes: 5
    steps:
      - uses: actions/checkout@v4
      - run: node scripts/plan.mjs
`)
	writeWorkflow(t, dir, "README.md", "not a workflow")

	report, err := lintWorkflows(lintOptions{dir: dir, planner: "workspace-impact.yml", tinySteps: 2, tinyTimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	kinds := findingKinds(report)
	if report.Summary.Workflows != 3 {
		t.Fatalf("workflows = %d, want 3 (the README is not one)", report.Summary.Workflows)
	}
	if report.Summary.PlannerCallers != 2 || kinds["planner-duplicated"] != 1 {
		t.Fatalf("planner callers = %d, planner-duplicated findings = %d; report %#v", report.Summary.PlannerCallers, kinds["planner-duplicated"], report.Findings)
	}
	// gate (one step, 2 min) and impact (two steps, 5 min) are tiny; unit has a matrix and 20 min; typecheck has three steps and no timeout.
	if kinds["tiny-job"] != 2 {
		t.Fatalf("tiny-job findings = %d, want 2 (gate, impact); %#v", kinds["tiny-job"], report.Findings)
	}
	// typecheck has no timeout-minutes.
	if kinds["unbounded-job"] != 1 {
		t.Fatalf("unbounded-job findings = %d, want 1 (typecheck); %#v", kinds["unbounded-job"], report.Findings)
	}
	// web-ci.yml runs on every pull request; daemon-ci.yml has paths; workspace-impact.yml is workflow_call.
	if kinds["unfiltered-pull-request"] != 1 {
		t.Fatalf("unfiltered-pull-request findings = %d, want 1 (web-ci.yml); %#v", kinds["unfiltered-pull-request"], report.Findings)
	}
	// Jobs reachable from a pull request: unit, gate (daemon-ci) + typecheck (web-ci); planner calls are not machines of their own.
	if report.Summary.PullRequestJobs != 3 {
		t.Fatalf("pull request jobs = %d, want 3", report.Summary.PullRequestJobs)
	}
	var text strings.Builder
	writeLintReport(&text, report)
	for _, want := range []string{"planner callers 2", "[planner-duplicated]", "[tiny-job] daemon-ci.yml:gate", "[unbounded-job] web-ci.yml:typecheck", "[unfiltered-pull-request] web-ci.yml", "remedy:"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("report is missing %q:\n%s", want, text.String())
		}
	}
}

func TestLintWorkflowsReadsEveryShapeOfOn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "list.yml", "name: list\non: [push, pull_request]\njobs:\n  a:\n    runs-on: x\n    timeout-minutes: 10\n    steps: [{run: a}, {run: b}, {run: c}]\n")
	writeWorkflow(t, dir, "scalar.yml", "name: scalar\non: pull_request\njobs:\n  a:\n    runs-on: x\n    timeout-minutes: 10\n    steps: [{run: a}, {run: b}, {run: c}]\n")
	writeWorkflow(t, dir, "ignore.yml", "name: ignore\non:\n  pull_request:\n    paths-ignore: ['docs/**']\njobs:\n  a:\n    runs-on: x\n    timeout-minutes: 10\n    steps: [{run: a}, {run: b}, {run: c}]\n")
	writeWorkflow(t, dir, "push-only.yml", "name: push\non:\n  push:\n    branches: [main]\njobs:\n  a:\n    runs-on: x\n    timeout-minutes: 10\n    steps: [{run: a}, {run: b}, {run: c}]\n")
	report, err := lintWorkflows(lintOptions{dir: dir, planner: "workspace-impact.yml", tinySteps: 2, tinyTimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	kinds := findingKinds(report)
	if kinds["unfiltered-pull-request"] != 2 {
		t.Fatalf("list and scalar forms must both count as unfiltered pull_request triggers, got %d: %#v", kinds["unfiltered-pull-request"], report.Findings)
	}
	if report.Summary.PullRequestJobs != 3 {
		t.Fatalf("pull request jobs = %d, want 3 (list, scalar, ignore)", report.Summary.PullRequestJobs)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("no other finding expected on well-formed jobs, got %#v", report.Findings)
	}
}

func TestLintWorkflowsReportsAnUnparseableFileInsteadOfStopping(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "broken.yml", "name: [unclosed\non: pull_request\n")
	writeWorkflow(t, dir, "fine.yml", "name: fine\non:\n  push:\njobs:\n  a:\n    runs-on: x\n    timeout-minutes: 10\n    steps: [{run: a}, {run: b}, {run: c}]\n")
	report, err := lintWorkflows(lintOptions{dir: dir, planner: "workspace-impact.yml", tinySteps: 2, tinyTimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if findingKinds(report)["unparseable"] != 1 || report.Summary.Workflows != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestLintWorkflowsAcceptsAPullRequestTriggerThatNeverFiresOnCode(t *testing.T) {
	dir := t.TempDir()
	// A gate that only means something on a label: it never runs on the push every PR makes.
	writeWorkflow(t, dir, "release-gates.yml", "name: gates\non:\n  pull_request:\n    types: [labeled]\njobs:\n  gate:\n    runs-on: x\n    timeout-minutes: 60\n    steps:\n      - uses: actions/checkout@v4\n      - run: pnpm gate\n      - run: pnpm evidence\n")
	// The same list WITH a code event is still every pull request.
	writeWorkflow(t, dir, "ci.yml", "name: ci\non:\n  pull_request:\n    types: [opened, synchronize, labeled]\njobs:\n  test:\n    runs-on: x\n    timeout-minutes: 60\n    steps:\n      - uses: actions/checkout@v4\n      - run: pnpm test\n      - run: pnpm lint\n")
	report, err := lintWorkflows(lintOptions{dir: dir, planner: "workspace-impact.yml", tinySteps: 2, tinyTimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	var unfiltered []string
	for _, f := range report.Findings {
		if f.Kind == "unfiltered-pull-request" {
			unfiltered = append(unfiltered, f.Workflow)
		}
	}
	if len(unfiltered) != 1 || unfiltered[0] != "ci.yml" {
		t.Fatalf("only the workflow that fires on a code event runs on every pull request, got %v", unfiltered)
	}
}

func TestLintWorkflowsAcceptsAWorkflowThatPlansFromTheMergeBase(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ci.yml", "name: ci\non:\n  pull_request:\njobs:\n  guards:\n    runs-on: x\n    timeout-minutes: 10\n    steps:\n      - uses: actions/checkout@v4\n      - run: from=\"$(git merge-base \"$BASE\" \"$HEAD\")\"\n      - run: node guards.mjs\n")
	report, err := lintWorkflows(lintOptions{dir: dir, planner: "workspace-impact.yml", tinySteps: 2, tinyTimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if findingKinds(report)["unfiltered-pull-request"] != 0 {
		t.Fatalf("a workflow that plans from the merge-base answers per lane; paths would be redundant: %#v", report.Findings)
	}
}
