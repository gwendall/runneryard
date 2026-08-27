package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gwendall/runneryard/githubapp"
)

func runAuth(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printAuthHelp()
		return nil
	}
	if args[0] != "github" {
		return fmt.Errorf("unknown auth provider %q; run runneryard auth --help", args[0])
	}
	args = args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printGitHubAuthHelp()
		return nil
	}
	switch args[0] {
	case "create":
		return runGitHubAppCreate(args[1:])
	case "import":
		return runGitHubAppImport(args[1:])
	default:
		return fmt.Errorf("unknown GitHub auth command %q; run runneryard auth github --help", args[0])
	}
}

func printAuthHelp() {
	fmt.Print(`RunnerYard authentication setup.

Usage:
  runneryard auth github create [flags]
  runneryard auth github import [flags]
`)
}

func printGitHubAuthHelp() {
	fmt.Print(`Configure the dedicated GitHub App used by a RunnerYard controller.

Usage:
  runneryard auth github create [flags]  Create an owner-controlled app in the browser
  runneryard auth github import [flags]  Verify and store an existing app from a PEM file

The create flow uses GitHub's manifest handshake. RunnerYard never receives the
private key and no token needs to be copied into the terminal.
`)
}

type authOptions struct {
	directory     string
	githubURL     string
	ownerKind     string
	sink          string
	controllerApp string
	force         bool
	noBrowser     bool
	timeout       time.Duration
}

func addAuthFlags(flags *flag.FlagSet, options *authOptions) {
	flags.StringVar(&options.directory, "directory", ".", "repository directory and local credential destination")
	flags.StringVar(&options.githubURL, "github", "", "GitHub repository or organization URL")
	flags.StringVar(&options.ownerKind, "owner-kind", "auto", "auto, user, or organization")
	flags.StringVar(&options.sink, "sink", "auto", "auto, fly, or file")
	flags.StringVar(&options.controllerApp, "controller-app", "", "Fly controller app for the fly secret sink")
	flags.BoolVar(&options.force, "force", false, "replace existing local credentials during an intentional rotation")
}

func runGitHubAppCreate(args []string) error {
	flags := flag.NewFlagSet("auth github create", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	options := authOptions{}
	addAuthFlags(flags, &options)
	flags.BoolVar(&options.noBrowser, "no-browser", false, "print setup URLs instead of opening a browser")
	flags.DurationVar(&options.timeout, "timeout", 10*time.Minute, "maximum time for browser approval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target, err := authTarget(context.Background(), options)
	if err != nil {
		return err
	}
	sink, err := authSink(options)
	if err != nil {
		return err
	}
	result, err := githubapp.Bootstrap(context.Background(), githubapp.Options{
		Target: target, Sink: sink, Browser: systemBrowser{}, NoBrowser: options.noBrowser,
		Timeout: options.timeout, HTTPClient: githubHTTPClient(), Output: os.Stdout,
	})
	if err != nil {
		return err
	}
	fmt.Printf("GitHub App installed and verified: %s\n", result.AppURL)
	fmt.Printf("Credentials stored in %s. No private key was printed or sent to RunnerYard.\n", result.SinkDescription)
	return nil
}

func runGitHubAppImport(args []string) error {
	flags := flag.NewFlagSet("auth github import", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	options := authOptions{}
	addAuthFlags(flags, &options)
	clientID := flags.String("client-id", "", "GitHub App client ID or numeric app ID")
	installationID := flags.Int64("installation-id", 0, "GitHub App installation ID")
	privateKeyFile := flags.String("private-key-file", "", "path to an existing GitHub App PEM private key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*clientID) == "" || *installationID < 1 || strings.TrimSpace(*privateKeyFile) == "" {
		return errors.New("--client-id, --installation-id, and --private-key-file are required")
	}
	privateKey, err := readPrivateKeyFile(*privateKeyFile)
	if err != nil {
		return err
	}
	credentials := githubapp.Credentials{
		ClientID: strings.TrimSpace(*clientID), InstallationID: *installationID, PrivateKey: privateKey,
	}
	target, err := authTarget(context.Background(), options)
	if err != nil {
		return err
	}
	api := githubapp.API{Client: githubHTTPClient()}
	if err := api.Verify(context.Background(), credentials, target); err != nil {
		return err
	}
	sink, err := authSink(options)
	if err != nil {
		return err
	}
	if err := sink.Store(context.Background(), credentials); err != nil {
		return err
	}
	fmt.Printf("Existing GitHub App verified for %s.\n", target.ConfigURL)
	fmt.Printf("Credentials stored in %s. No private key was printed or sent to RunnerYard.\n", sink.Description())
	return nil
}

func authTarget(ctx context.Context, options authOptions) (githubapp.Target, error) {
	githubURL := options.githubURL
	if githubURL == "" {
		githubURL = inferGitHubURL(options.directory)
	}
	if githubURL == "" {
		return githubapp.Target{}, errors.New("--github is required outside a GitHub checkout")
	}
	normalized, owner, err := normalizeGitHubConfigURL(githubURL)
	if err != nil {
		return githubapp.Target{}, fmt.Errorf("--github: %w", err)
	}
	parsed, _ := url.Parse(normalized)
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	repository := ""
	if len(parts) == 2 {
		repository = parts[1]
	}
	kind := githubapp.OwnerKind(options.ownerKind)
	if options.ownerKind == "auto" {
		api := githubapp.API{Client: githubHTTPClient()}
		kind, err = api.ResolveOwnerKind(ctx, owner)
		if err != nil {
			return githubapp.Target{}, err
		}
	}
	target := githubapp.Target{ConfigURL: normalized, Owner: owner, Repository: repository, OwnerKind: kind}
	if err := target.Validate(); err != nil {
		return githubapp.Target{}, err
	}
	return target, nil
}

func authSink(options authOptions) (githubapp.SecretSink, error) {
	sinkName := options.sink
	if sinkName == "auto" {
		if options.controllerApp != "" {
			sinkName = "fly"
		} else {
			sinkName = "file"
		}
	}
	switch sinkName {
	case "fly":
		if options.controllerApp == "" {
			return nil, errors.New("--controller-app is required with --sink fly")
		}
		return githubapp.FlySink{App: options.controllerApp}, nil
	case "file":
		return githubapp.FileSink{ProjectDirectory: options.directory, Force: options.force}, nil
	default:
		return nil, fmt.Errorf("unsupported credential sink %q", sinkName)
	}
}

func readPrivateKeyFile(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("resolve private key path")
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return "", errors.New("open private key directory")
	}
	defer root.Close()
	name := filepath.Base(absolute)
	info, err := root.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("inspect private key file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("private key path must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("private key file is readable by group or others; run chmod 600 first")
	}
	if info.Size() < 1 || info.Size() > 64<<10 {
		return "", errors.New("private key file size is outside the safe range")
	}
	// Root scopes the open to the inspected directory; Lstat rejects symlinks.
	file, err := root.Open(name)
	if err != nil {
		return "", errors.New("open private key file")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", errors.New("private key file changed while it was being inspected")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return "", errors.New("read private key file")
	}
	return string(contents), nil
}

type systemBrowser struct{}

func (systemBrowser) Open(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target) // #nosec G204 -- one URL argument, no shell
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target) // #nosec G204 -- one URL argument, no shell
	default:
		command = exec.Command("xdg-open", target) // #nosec G204 -- one URL argument, no shell
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

func githubHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("GitHub API redirects are refused during credential setup")
		},
	}
}
