package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gwendall/runneryard/mergequeue"
)

func runMergeQueue(args []string) error {
	if len(args) == 0 || args[0] != "tick" {
		if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
			printMergeQueueHelp(os.Stdout)
			return nil
		}
		printMergeQueueHelp(os.Stderr)
		return errors.New("usage: runneryard merge-queue tick [flags]")
	}
	return runMergeQueueTick(args[1:], os.Stdout, os.Getenv)
}

func printMergeQueueHelp(output io.Writer) {
	fmt.Fprint(output, `Usage: runneryard merge-queue tick [flags]

Runs one tick of the merge queue for one repository, inside a GitHub Actions
workflow. It never runs in the fleet controller and never uses its
credentials. See docs/merge-queue.md.

Environment:
  MERGE_QUEUE_TOKEN      installation token of the queue's own GitHub App
  MERGE_QUEUE_BOT_LOGIN  that App's bot login, "<app-slug>[bot]"
  GITHUB_TOKEN           without MERGE_QUEUE_TOKEN: plan only, write nothing
  GITHUB_REPOSITORY, GITHUB_API_URL, GITHUB_SERVER_URL, RUNNER_TEMP
`)
}

func runMergeQueueTick(args []string, output io.Writer, getenv func(string) string) error {
	flags := flag.NewFlagSet("merge-queue tick", flag.ContinueOnError)
	flags.SetOutput(output)
	repository := flags.String("repo", getenv("GITHUB_REPOSITORY"), "repository as owner/name")
	dryRun := flags.Bool("dry-run", false, "print the plan and write nothing")
	botLogin := flags.String("bot-login", getenv("MERGE_QUEUE_BOT_LOGIN"), `the queue App's bot login, "<app-slug>[bot]"`)
	settle := flags.Duration("settle", 20*time.Second, "how long a tick that formed a stack keeps cancelling the runs it started on batchmates")
	timeout := flags.Duration("timeout", 10*time.Minute, "upper bound for the whole tick")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("merge-queue tick does not accept positional arguments")
	}
	owner, repo, ok := strings.Cut(*repository, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return fmt.Errorf("--repo must be owner/name, got %q", *repository)
	}
	if *settle < 0 || *timeout <= 0 {
		return errors.New("--settle must not be negative and --timeout must be positive")
	}

	token := getenv("MERGE_QUEUE_TOKEN")
	if token == "" {
		token = getenv("GITHUB_TOKEN")
		if token == "" {
			return errors.New("set MERGE_QUEUE_TOKEN to the queue App's installation token, or GITHUB_TOKEN for a dry run")
		}
		if !*dryRun {
			fmt.Fprintln(output, "MERGE_QUEUE_TOKEN is not set: dry run with GITHUB_TOKEN, nothing is written.")
			*dryRun = true
		}
	}
	apiURL := strings.TrimRight(envDefault(getenv, "GITHUB_API_URL", "https://api.github.com"), "/")
	serverURL := strings.TrimRight(envDefault(getenv, "GITHUB_SERVER_URL", "https://github.com"), "/")
	remote := serverURL + "/" + owner + "/" + repo + ".git"

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	workdir := ""
	defer func() {
		if workdir != "" {
			_ = os.RemoveAll(workdir)
		}
	}()
	options := mergequeue.Options{
		GitHub:     &mergequeue.RESTClient{Owner: owner, Repo: repo, Token: token, BaseURL: apiURL},
		BotLogin:   *botLogin,
		DryRun:     *dryRun,
		SettleWait: *settle,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(output, "  "+format+"\n", args...)
		},
		OpenGit: func(ctx context.Context, target string) (mergequeue.Git, error) {
			dir, err := os.MkdirTemp(getenv("RUNNER_TEMP"), "runneryard-merge-queue-")
			if err != nil {
				return nil, err
			}
			workdir = dir
			return mergequeue.NewLocalGit(ctx, filepath.Join(dir, "repo"), remote, target, token)
		},
	}
	plan, report, err := mergequeue.Tick(ctx, options)
	writeMergeQueuePlan(output, *repository, plan, *dryRun)
	if err != nil {
		return fmt.Errorf("merge queue tick: %w", err)
	}
	if !*dryRun {
		writeMergeQueueReport(output, report)
	}
	return nil
}

func envDefault(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

func writeMergeQueuePlan(output io.Writer, repository string, plan mergequeue.Plan, dryRun bool) {
	verb := "Plan"
	if dryRun {
		verb = "Plan (dry run)"
	}
	fmt.Fprintf(output, "%s for %s: %d action(s)\n", verb, repository, len(plan.Actions))
	for _, action := range plan.Actions {
		fmt.Fprintf(output, "- %s\n", action)
	}
	if plan.Idle {
		fmt.Fprintln(output, "Nothing to land or form on this tick.")
	}
}

func writeMergeQueueReport(output io.Writer, report mergequeue.Report) {
	for _, line := range []struct {
		label  string
		stacks [][]int
	}{
		{"Formed", report.Formed},
		{"Landed", report.Landed},
		{"Retried", report.Retried},
		{"Split", report.Split},
		{"Abandoned", report.Abandoned},
		{"Deferred", report.Deferred},
	} {
		if len(line.stacks) > 0 {
			fmt.Fprintf(output, "%-9s %s\n", line.label, stackList(line.stacks))
		}
	}
	if len(report.Rejected) > 0 {
		fmt.Fprintf(output, "%-9s %s\n", "Rejected", stackList([][]int{report.Rejected}))
	}
}

func stackList(stacks [][]int) string {
	parts := make([]string, 0, len(stacks))
	for _, stack := range stacks {
		numbers := make([]string, 0, len(stack))
		for _, number := range stack {
			numbers = append(numbers, fmt.Sprintf("#%d", number))
		}
		parts = append(parts, strings.Join(numbers, " "))
	}
	return strings.Join(parts, ", ")
}
