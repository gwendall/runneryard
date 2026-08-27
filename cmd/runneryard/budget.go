package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/gwendall/runneryard/controller"
)

func runBudget(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return fmt.Errorf("usage: runneryard budget init --file PATH")
	}
	flags := flag.NewFlagSet("budget init", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	stateFile := flags.String("file", strings.TrimSpace(os.Getenv("RUNNER_BUDGET_FILE")), "durable usage ledger path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if err := controller.InitializeUsageBudget(*stateFile); err != nil {
		return err
	}
	fmt.Printf("Initialized runner usage budget at %s\n", *stateFile)
	return nil
}
