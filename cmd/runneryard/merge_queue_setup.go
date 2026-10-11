package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gwendall/runneryard/githubapp"
	"github.com/gwendall/runneryard/mergequeue"
)

// mergeQueueAppName starts the queue App's name. GitHub caps App names at
// 34 characters, so it leaves room for the random suffix and no more.
const mergeQueueAppName = "RunnerYard merge queue"

// mergeQueueSetup holds what setup reaches outside the process, so tests can
// replace it.
type mergeQueueSetup struct {
	target    func(context.Context, authOptions) (githubapp.Target, error)
	runGH     githubapp.CommandRunner
	bootstrap func(context.Context, githubapp.Options) (githubapp.Result, error)
	reuse     func(context.Context, githubapp.Options, githubapp.Credentials) (githubapp.Result, error)
	readKey   func(string) (string, error)
	browser   githubapp.Browser
}

func defaultMergeQueueSetup() mergeQueueSetup {
	return mergeQueueSetup{
		target:    authTarget,
		bootstrap: githubapp.Bootstrap,
		reuse:     githubapp.Reuse,
		readKey:   readPrivateKeyFile,
		browser:   systemBrowser{},
	}
}

func mergeQueueProfile(workflows bool) githubapp.Profile {
	explanation := "It requests Contents, Pull requests, Issues and Actions: write and Checks: read, to stack, test and land queued pull requests."
	if workflows {
		explanation += " With --workflows it also requests Workflows: write, to land pull requests that change .github/workflows."
	}
	return githubapp.Profile{
		Name:        mergeQueueAppName,
		Description: "RunnerYard merge queue: stacks, tests and fast-forwards labeled pull requests from a GitHub Actions workflow.",
		Permissions: mergequeue.AppPermissions(workflows),
		Explanation: explanation,
	}
}

func printMergeQueueSetupHelp(output io.Writer) {
	fmt.Fprint(output, `Usage: runneryard merge-queue setup [flags]

Creates the merge queue's own GitHub App in the browser, installs it on one
repository, verifies the installation, and stores what the Merge queue
workflow reads: the Actions variable `+mergequeue.AppClientIDVariable+` and the
Actions secret `+mergequeue.AppPrivateKeySecret+`. The private key goes from
GitHub to the secret through gh; it is never printed or written to disk.

The App is separate from the runner App. To serve another repository of the
same owner with it, generate a new private key in the App's settings, then:
  runneryard merge-queue setup --github <repository URL> --client-id <client ID> --private-key-file <key.pem>

Requires gh, logged in as an administrator of the repository.

Flags:
`)
}

func runMergeQueueSetup(args []string, output io.Writer, setup mergeQueueSetup) error {
	flags := flag.NewFlagSet("merge-queue setup", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		printMergeQueueSetupHelp(output)
		flags.PrintDefaults()
	}
	options := authOptions{ownerKind: "auto"}
	flags.StringVar(&options.directory, "directory", ".", "checkout whose GitHub remote names the repository when --github is not set")
	flags.StringVar(&options.githubURL, "github", "", "repository URL, https://github.com/<owner>/<repository>")
	flags.BoolVar(&options.noBrowser, "no-browser", false, "print setup URLs instead of opening a browser")
	flags.DurationVar(&options.timeout, "timeout", 10*time.Minute, "maximum time for browser approval")
	workflows := flags.Bool("workflows", false, "also request Workflows: write, to land pull requests that change .github/workflows")
	clientID := flags.String("client-id", "", "reuse this existing queue App instead of creating one")
	privateKeyFile := flags.String("private-key-file", "", "a private key of the App given by --client-id")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("merge-queue setup does not accept positional arguments")
	}
	reusing := strings.TrimSpace(*clientID) != "" || strings.TrimSpace(*privateKeyFile) != ""
	if reusing && (strings.TrimSpace(*clientID) == "" || strings.TrimSpace(*privateKeyFile) == "") {
		return errors.New("--client-id and --private-key-file go together, to reuse an existing queue App")
	}

	ctx := context.Background()
	target, err := setup.target(ctx, options)
	if err != nil {
		return err
	}
	if target.Repository == "" {
		return errors.New("the merge queue serves one repository: pass --github https://github.com/<owner>/<repository>")
	}
	sink := githubapp.RepositorySink{
		Owner: target.Owner, Repository: target.Repository,
		ClientIDVariable: mergequeue.AppClientIDVariable, PrivateKeySecret: mergequeue.AppPrivateKeySecret,
		Run: setup.runGH,
	}
	// Before creating anything: GitHub shows a new App's key only once.
	if err := sink.Ready(ctx); err != nil {
		return err
	}
	profile := mergeQueueProfile(*workflows)
	appOptions := githubapp.Options{
		Target: target, Profile: &profile, Sink: sink, Browser: setup.browser, NoBrowser: options.noBrowser,
		Timeout: options.timeout, HTTPClient: githubHTTPClient(), Output: output,
	}
	var result githubapp.Result
	if reusing {
		privateKey, err := setup.readKey(*privateKeyFile)
		if err != nil {
			return err
		}
		result, err = setup.reuse(ctx, appOptions, githubapp.Credentials{ClientID: strings.TrimSpace(*clientID), PrivateKey: privateKey})
		if err != nil {
			return err
		}
	} else if result, err = setup.bootstrap(ctx, appOptions); err != nil {
		return err
	}

	fmt.Fprintf(output, "Merge queue App installed and verified on %s: %s\n", target.ConfigURL, result.AppURL)
	if result.Slug != "" {
		fmt.Fprintf(output, "Its bot login, the only author whose queue markers count: %s[bot]\n", result.Slug)
	}
	fmt.Fprintf(output, "Credentials stored in %s. No private key was printed or written to disk.\n", result.SinkDescription)
	if result.AllRepositories {
		fmt.Fprintf(output, "Warning: the installation covers every repository of %s. Limit it to the repositories the queue serves in the App's installation settings.\n", target.Owner)
	}
	if *workflows {
		fmt.Fprintln(output, "Uncomment permission-workflows: write in the workflow's token step.")
	}
	fmt.Fprintln(output, sink.NextStep())
	return nil
}
