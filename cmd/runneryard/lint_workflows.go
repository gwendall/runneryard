package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// lint-workflows reads a repository's .github/workflows and says what its shape
// will cost on an ephemeral fleet, before a single machine is launched.
//
// Every job is a machine: created, booted, registered (about a minute), used,
// destroyed. The shape of the workflows decides how many machines a pull request
// starts and how many of those do real work. Measured on one monorepo on
// 2026-09-05: 3 753 fleet jobs in a day with a MEDIAN duration of six seconds,
// 90 hours of job time for 292 machine-hours billed, 66 jobs for a one-file pull
// request, the same planner job copied into fourteen workflows. None of that was a
// runner defect; all of it was visible in the YAML. This command makes it visible
// on day one of a new repository, and measurable on a repository that drifted.
//
// The lint is offline and advisory: it reads YAML, never the API, and exits 0
// unless --strict is set. Findings name the file, the job and the remedy.

type lintFinding struct {
	Kind     string `json:"kind"`
	Workflow string `json:"workflow"`
	Job      string `json:"job,omitempty"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy"`
}

type lintSummary struct {
	Workflows          int `json:"workflows"`
	Jobs               int `json:"jobs"`
	PullRequestJobs    int `json:"pull_request_jobs"`
	PlannerCallers     int `json:"planner_callers"`
	TinyJobs           int `json:"tiny_jobs"`
	UnboundedJobs      int `json:"unbounded_jobs"`
	UnfilteredPRTriggr int `json:"unfiltered_pull_request_workflows"`
}

type lintReport struct {
	Directory string        `json:"directory"`
	Summary   lintSummary   `json:"summary"`
	Findings  []lintFinding `json:"findings"`
}

type lintOptions struct {
	dir         string
	planner     string
	tinySteps   int
	tinyTimeout int
}

func runLintWorkflows(args []string) error {
	flags := flag.NewFlagSet("lint-workflows", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	dir := flags.String("dir", ".github/workflows", "directory holding the workflow files")
	planner := flags.String("planner", "workspace-impact.yml", "file name of the reusable planner workflow whose callers are counted")
	tinySteps := flags.Int("tiny-steps", 2, "a job with this many steps or fewer (and no matrix) is a tiny job: a machine for seconds of work")
	tinyTimeout := flags.Int("tiny-timeout", 5, "a job whose timeout-minutes is this or lower is also a tiny job")
	asJSON := flags.Bool("json", false, "emit the report as JSON")
	strict := flags.Bool("strict", false, "exit 1 when the lint has findings")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("lint-workflows does not accept positional arguments")
	}
	report, err := lintWorkflows(lintOptions{dir: *dir, planner: *planner, tinySteps: *tinySteps, tinyTimeout: *tinyTimeout})
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		writeLintReport(os.Stdout, report)
	}
	if *strict && len(report.Findings) > 0 {
		return fmt.Errorf("lint-workflows: %d finding(s)", len(report.Findings))
	}
	return nil
}

func lintWorkflows(opts lintOptions) (lintReport, error) {
	entries, err := os.ReadDir(opts.dir)
	if err != nil {
		return lintReport{}, fmt.Errorf("read %s: %w", opts.dir, err)
	}
	report := lintReport{Directory: opts.dir, Findings: []lintFinding{}}
	plannerCallers := []string{}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(opts.dir, name))
		if err != nil {
			return lintReport{}, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			report.Findings = append(report.Findings, lintFinding{Kind: "unparseable", Workflow: name, Detail: err.Error(), Remedy: "fix the YAML; a workflow GitHub cannot parse runs nothing and says little"})
			continue
		}
		root := documentRoot(&doc)
		if root == nil {
			continue
		}
		report.Summary.Workflows++
		triggers := mappingValue(root, "on")
		onPullRequest, prFiltered := pullRequestTrigger(triggers)
		if onPullRequest && !prFiltered {
			report.Summary.UnfilteredPRTriggr++
			report.Findings = append(report.Findings, lintFinding{
				Kind: "unfiltered-pull-request", Workflow: name,
				Detail: "pull_request trigger without paths or paths-ignore: this workflow runs on every pull request",
				Remedy: "add `paths:` naming what the workflow proves, or route it through the planner",
			})
		}
		jobs := mappingValue(root, "jobs")
		if jobs == nil || jobs.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			jobID := jobs.Content[i].Value
			job := jobs.Content[i+1]
			if job.Kind != yaml.MappingNode {
				continue
			}
			if uses := scalarValue(mappingValue(job, "uses")); uses != "" {
				if strings.HasSuffix(uses, "/"+opts.planner) || uses == "./"+opts.planner {
					plannerCallers = append(plannerCallers, name+":"+jobID)
				}
				// A reusable-workflow call is not a machine of its own: its jobs are counted where they run.
				continue
			}
			report.Summary.Jobs++
			if onPullRequest {
				report.Summary.PullRequestJobs++
			}
			steps := mappingValue(job, "steps")
			stepCount := 0
			if steps != nil && steps.Kind == yaml.SequenceNode {
				stepCount = len(steps.Content)
			}
			hasMatrix := mappingValue(mappingValue(job, "strategy"), "matrix") != nil
			timeout, hasTimeout := intValue(mappingValue(job, "timeout-minutes"))
			if !hasTimeout {
				report.Summary.UnboundedJobs++
				report.Findings = append(report.Findings, lintFinding{
					Kind: "unbounded-job", Workflow: name, Job: jobID,
					Detail: "no timeout-minutes: GitHub's default is 360, so a hung job holds a machine for six hours",
					Remedy: "set timeout-minutes from a measured p95 with a margin",
				})
			}
			tiny := !hasMatrix && ((stepCount > 0 && stepCount <= opts.tinySteps) || (hasTimeout && timeout <= opts.tinyTimeout))
			if tiny {
				report.Summary.TinyJobs++
				report.Findings = append(report.Findings, lintFinding{
					Kind: "tiny-job", Workflow: name, Job: jobID,
					Detail: fmt.Sprintf("%d step(s), timeout %s: seconds of work on a machine that takes about a minute to create, boot and register", stepCount, timeoutLabel(timeout, hasTimeout)),
					Remedy: "make it a step of a neighbouring job, or group the small guards into one job",
				})
			}
		}
	}
	report.Summary.PlannerCallers = len(plannerCallers)
	if len(plannerCallers) > 1 {
		report.Findings = append(report.Findings, lintFinding{
			Kind:     "planner-duplicated",
			Workflow: opts.planner,
			Detail:   fmt.Sprintf("%d jobs call the same planner (%s): each caller is a machine that recomputes the same answer", len(plannerCallers), strings.Join(plannerCallers, ", ")),
			Remedy:   "compute the impact once per event and hand the result to the callers, or merge the callers into fewer workflows",
		})
	}
	return report, nil
}

func writeLintReport(w io.Writer, report lintReport) {
	s := report.Summary
	fmt.Fprintf(w, "lint-workflows %s\n", report.Directory)
	fmt.Fprintf(w, "  workflows %d, jobs %d (%d reachable from a pull request), planner callers %d\n", s.Workflows, s.Jobs, s.PullRequestJobs, s.PlannerCallers)
	fmt.Fprintf(w, "  tiny jobs %d, unbounded jobs %d, pull_request workflows without a path filter %d\n", s.TinyJobs, s.UnboundedJobs, s.UnfilteredPRTriggr)
	if len(report.Findings) == 0 {
		fmt.Fprintln(w, "  no findings")
		return
	}
	fmt.Fprintf(w, "  %d finding(s):\n", len(report.Findings))
	for _, f := range report.Findings {
		where := f.Workflow
		if f.Job != "" {
			where += ":" + f.Job
		}
		fmt.Fprintf(w, "  - [%s] %s: %s\n      remedy: %s\n", f.Kind, where, f.Detail, f.Remedy)
	}
}

// ── YAML helpers: the document is read as nodes so `on` stays a key, whatever a
// YAML 1.1 reader would make of it, and every shape of `on` (string, list, map)
// is handled without a schema. ──

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func scalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func intValue(node *yaml.Node) (int, bool) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return 0, false
	}
	var v int
	if err := node.Decode(&v); err != nil {
		return 0, false
	}
	return v, true
}

func timeoutLabel(timeout int, has bool) string {
	if !has {
		return "unset"
	}
	return fmt.Sprintf("%d min", timeout)
}

// pullRequestTrigger reports whether the workflow runs on pull_request and, if so,
// whether that trigger carries a paths or paths-ignore filter.
func pullRequestTrigger(on *yaml.Node) (present bool, filtered bool) {
	if on == nil {
		return false, false
	}
	switch on.Kind {
	case yaml.ScalarNode:
		return on.Value == "pull_request", false
	case yaml.SequenceNode:
		for _, item := range on.Content {
			if item.Value == "pull_request" {
				return true, false
			}
		}
		return false, false
	case yaml.MappingNode:
		pr := mappingValue(on, "pull_request")
		if pr == nil {
			for i := 0; i+1 < len(on.Content); i += 2 {
				if on.Content[i].Value == "pull_request" {
					return true, false
				}
			}
			return false, false
		}
		return true, mappingValue(pr, "paths") != nil || mappingValue(pr, "paths-ignore") != nil
	}
	return false, false
}
