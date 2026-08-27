package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultRouteVariable = "CI_LINUX_RUNNER"
	hostedLinuxRunner    = "ubuntu-latest"
)

type routeCommandRunner func(context.Context, []string, []byte) ([]byte, error)

type routeOptions struct {
	githubURL     string
	variable      string
	label         string
	dryRun        bool
	confirmCanary bool
	json          bool
}

type routeState struct {
	Repository string `json:"repository"`
	Variable   string `json:"variable"`
	Mode       string `json:"mode"`
	Runner     string `json:"runner"`
	Exists     bool   `json:"variable_exists"`
}

type routeReceipt struct {
	Action     string `json:"action"`
	Changed    bool   `json:"changed"`
	DryRun     bool   `json:"dry_run"`
	Repository string `json:"repository"`
	Variable   string `json:"variable"`
	Runner     string `json:"runner"`
}

func runRoute(args []string) error {
	return runRouteWith(args, execRouteCommand, os.Stdout)
}

func runRouteWith(args []string, run routeCommandRunner, output io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printRouteHelp(output)
		return nil
	}
	command := args[0]
	if command != "status" && command != "enable" && command != "disable" {
		return fmt.Errorf("unknown route command %q; run runneryard route --help", command)
	}
	flags := flag.NewFlagSet("route "+command, flag.ContinueOnError)
	flags.SetOutput(output)
	options := routeOptions{}
	flags.StringVar(&options.githubURL, "github", "", "exact GitHub repository URL")
	flags.StringVar(&options.variable, "variable", defaultRouteVariable, "repository variable used by runs-on")
	flags.BoolVar(&options.json, "json", false, "emit a machine-readable receipt")
	if command != "status" {
		flags.BoolVar(&options.dryRun, "dry-run", false, "show the intended change without writing it")
	}
	if command == "enable" {
		flags.StringVar(&options.label, "label", "", "qualified RunnerYard scale-set label")
		flags.BoolVar(&options.confirmCanary, "confirm-canary", false, "confirm doctor and the canary passed for this label")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("route commands do not accept positional arguments")
	}
	repository, err := routeRepository(options.githubURL)
	if err != nil {
		return err
	}
	if !validRouteVariable(options.variable) {
		return errors.New("--variable must start with a letter or underscore and contain only uppercase letters, numbers, or underscores")
	}
	if command == "enable" {
		if !safeName.MatchString(options.label) || options.label == hostedLinuxRunner {
			return errors.New("--label must be a non-hosted runner label containing only letters, numbers, dots, underscores, or hyphens")
		}
		if !options.dryRun && !options.confirmCanary {
			return errors.New("refusing to enable the fleet without --confirm-canary after doctor and the canary pass")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := inspectRoute(ctx, repository, options.variable, run)
	if err != nil {
		return err
	}
	switch command {
	case "status":
		return writeRouteState(output, state, options.json)
	case "disable":
		return disableRoute(ctx, output, state, options, run)
	case "enable":
		return enableRoute(ctx, output, state, options, run)
	default:
		panic("validated route command")
	}
}

func validRouteVariable(value string) bool {
	if len(value) < 1 || len(value) > 100 {
		return false
	}
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func routeRepository(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("--github is required for routing changes; pass the exact repository URL")
	}
	normalized, _, err := normalizeGitHubConfigURL(raw)
	if err != nil {
		return "", fmt.Errorf("--github: %w", err)
	}
	parts := strings.Split(strings.TrimPrefix(normalized, "https://github.com/"), "/")
	if len(parts) != 2 {
		return "", errors.New("--github must identify one repository, not an organization")
	}
	return strings.Join(parts, "/"), nil
}

func inspectRoute(ctx context.Context, repository, variable string, run routeCommandRunner) (routeState, error) {
	state := routeState{Repository: repository, Variable: variable, Mode: "hosted", Runner: hostedLinuxRunner}
	response, err := run(ctx, []string{"variable", "get", variable, "--repo", repository, "--json", "value", "--jq", ".value"}, nil)
	if err == nil {
		value := strings.TrimSpace(string(response))
		if value == "" {
			return routeState{}, errors.New("GitHub returned an empty routing variable")
		}
		state.Exists = true
		state.Mode = "custom"
		state.Runner = value
		return state, nil
	}
	if !strings.Contains(string(response), "variable "+variable+" was not found") {
		return routeState{}, errors.New("cannot read the routing variable; check gh auth status and repository Variables permission")
	}
	if _, repositoryErr := run(ctx, []string{"repo", "view", repository, "--json", "name", "--jq", ".name"}, nil); repositoryErr != nil {
		return routeState{}, errors.New("repository is unavailable to the current gh identity")
	}
	return state, nil
}

func disableRoute(ctx context.Context, output io.Writer, current routeState, options routeOptions, run routeCommandRunner) error {
	receipt := routeReceipt{Action: "disable", DryRun: options.dryRun, Repository: current.Repository, Variable: current.Variable, Runner: hostedLinuxRunner}
	if !current.Exists {
		return writeRouteReceipt(output, receipt, options.json)
	}
	if options.dryRun {
		receipt.Changed = true
		return writeRouteReceipt(output, receipt, options.json)
	}
	response, err := run(ctx, []string{"variable", "delete", current.Variable, "--repo", current.Repository}, nil)
	if err != nil && !strings.Contains(string(response), "variable "+current.Variable+" was not found") {
		return errors.New("failed to remove the routing variable; hosted-runner failover was not confirmed")
	}
	verified, err := inspectRoute(ctx, current.Repository, current.Variable, run)
	if err != nil || verified.Exists {
		return errors.New("routing variable deletion could not be verified; inspect it with runneryard route status")
	}
	receipt.Changed = true
	return writeRouteReceipt(output, receipt, options.json)
}

func enableRoute(ctx context.Context, output io.Writer, current routeState, options routeOptions, run routeCommandRunner) error {
	receipt := routeReceipt{Action: "enable", DryRun: options.dryRun, Repository: current.Repository, Variable: current.Variable, Runner: options.label}
	if current.Exists && current.Runner == options.label {
		return writeRouteReceipt(output, receipt, options.json)
	}
	if options.dryRun {
		receipt.Changed = true
		return writeRouteReceipt(output, receipt, options.json)
	}
	if _, err := run(ctx, []string{"variable", "set", current.Variable, "--repo", current.Repository}, []byte(options.label)); err != nil {
		return errors.New("failed to set the routing variable; RunnerYard routing was not confirmed")
	}
	verified, err := inspectRoute(ctx, current.Repository, current.Variable, run)
	if err != nil || !verified.Exists || verified.Runner != options.label {
		return errors.New("routing variable update could not be verified; inspect it with runneryard route status")
	}
	receipt.Changed = true
	return writeRouteReceipt(output, receipt, options.json)
}

func writeRouteState(output io.Writer, state routeState, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(output).Encode(state)
	}
	if state.Exists {
		fmt.Fprintf(output, "%s routes %s through %q (%s).\n", state.Repository, state.Variable, state.Runner, state.Mode)
		return nil
	}
	fmt.Fprintf(output, "%s routes Linux jobs to %s because %s is absent.\n", state.Repository, hostedLinuxRunner, state.Variable)
	return nil
}

func writeRouteReceipt(output io.Writer, receipt routeReceipt, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(output).Encode(receipt)
	}
	verb := "already routes"
	if receipt.DryRun && receipt.Changed {
		verb = "would route"
	} else if receipt.Changed {
		verb = "now routes"
	}
	fmt.Fprintf(output, "%s %s Linux jobs to %s via %s.\n", receipt.Repository, verb, receipt.Runner, receipt.Variable)
	return nil
}

func execRouteCommand(ctx context.Context, arguments []string, stdin []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, "gh", arguments...) // #nosec G204 -- fixed executable and discrete validated arguments, no shell
	command.Stdin = bytes.NewReader(stdin)
	return command.CombinedOutput()
}

func printRouteHelp(output io.Writer) {
	fmt.Fprint(output, `Switch workflows that use vars.CI_LINUX_RUNNER between RunnerYard and GitHub-hosted Linux runners.

Usage:
  runneryard route status  --github https://github.com/OWNER/REPO
  runneryard route disable --github https://github.com/OWNER/REPO [--dry-run]
  runneryard route enable  --github https://github.com/OWNER/REPO --label LABEL --confirm-canary [--dry-run]

The command uses the existing local gh authentication. It never asks for or
prints a GitHub token. Disable removes the repository variable, activating the
ubuntu-latest fallback. Enable refuses to write until doctor and the canary have
been explicitly confirmed.
`)
}
