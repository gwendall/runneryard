package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gwendall/runneryard/controller"
)

var (
	version   = "dev"
	commitSHA = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "serve":
		return serve()
	case "init":
		return runInit(args)
	case "doctor":
		return runDoctor(args)
	case "budget":
		return runBudget(args)
	case "version", "--version", "-v":
		fmt.Printf("runneryard %s (%s)\n", version, commitSHA)
		return nil
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q; run runneryard help", command)
	}
}

func serve() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	github, err := cfg.githubClient()
	if err != nil {
		return fmt.Errorf("create GitHub adapter: %w", err)
	}
	compute, err := cfg.compute()
	if err != nil {
		return fmt.Errorf("create compute adapter: %w", err)
	}
	fleet, err := controller.New(cfg.controllerConfig(logger), github, compute)
	if err != nil {
		return fmt.Errorf("create controller: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := fleet.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("controller stopped: %w", err)
	}
	return nil
}

func printHelp() {
	fmt.Print(`RunnerYard starts one isolated GitHub Actions runner per job.

Usage:
  runneryard init [flags]     Create a safe starter configuration
  runneryard doctor [flags]   Check credentials, isolation, and tooling
  runneryard budget init      Initialize the durable usage ledger once
  runneryard serve            Run the fleet controller (default)
  runneryard version          Print version information

Run "runneryard <command> --help" for command-specific options.
`)
}
