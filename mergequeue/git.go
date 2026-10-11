package mergequeue

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Committer identity of the commits the queue rewrites. Authors are kept.
const (
	committerName  = "runneryard-merge-queue"
	committerEmail = "merge-queue@runneryard.invalid"
)

// inheritedEnvironment is all git gets from the tick's environment: enough
// to find programs, reach the network through a proxy, and trust the
// system's certificates. Tokens and the runner's own GIT_* settings stay out.
var inheritedEnvironment = []string{
	"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy",
}

// LocalGit implements Git with the git command line in a throwaway clone.
//
// The clone is hardened for a repository whose content the queue does not
// trust: hooks are disabled, the user's global and system configuration are
// ignored, LFS smudging is skipped, and the token reaches git only through
// an http.extraheader passed in the environment, never in a URL, an
// argument, or .git/config.
type LocalGit struct {
	Dir string
	env []string
}

// NewLocalGit clones the target branch of remote into dir, without blobs
// until a rebase needs them. token may be empty for a remote that needs no
// authentication.
func NewLocalGit(ctx context.Context, dir, remote, target, token string) (*LocalGit, error) {
	config := [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"core.fsmonitor", "false"},
		{"commit.gpgsign", "false"},
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
		{"user.name", committerName},
		{"user.email", committerEmail},
	}
	if token != "" {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return nil, errors.New("an authenticated remote must be an https URL")
		}
		credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		config = append(config, [2]string{"http." + parsed.Scheme + "://" + parsed.Host + "/.extraheader", "AUTHORIZATION: basic " + credentials})
	}
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	for _, name := range inheritedEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, entry := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, entry[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, entry[1]))
	}
	git := &LocalGit{Dir: dir, env: env}
	if _, err := git.runIn(ctx, "", "clone", "--quiet", "--filter=blob:none", "--single-branch", "--no-tags", "--branch", target, "--", remote, dir); err != nil {
		return nil, err
	}
	return git, nil
}

func (g *LocalGit) run(ctx context.Context, args ...string) (string, error) {
	return g.runIn(ctx, g.Dir, args...)
}

func (g *LocalGit) runIn(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed executable, discrete arguments, no shell
	command.Dir = dir
	command.Env = g.env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.String() + stderr.String(), &gitError{args: args[0], output: strings.TrimSpace(stdout.String() + "\n" + stderr.String()), err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

type gitError struct {
	args   string
	output string
	err    error
}

func (e *gitError) Error() string {
	output := e.output
	if len(output) > 2000 {
		output = output[:2000] + "..."
	}
	return fmt.Sprintf("git %s: %v: %s", e.args, e.err, output)
}

func (e *gitError) Unwrap() error { return e.err }

// Fetch implements Git.
func (g *LocalGit) Fetch(ctx context.Context, ref string) (string, error) {
	if _, err := g.run(ctx, "fetch", "--quiet", "--no-tags", "origin", ref); err != nil {
		return "", err
	}
	return g.run(ctx, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
}

// IsAncestor implements Git.
func (g *LocalGit) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, err := g.run(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// 1 means not an ancestor; anything else, such as an unknown
		// commit, cannot be shown to be one either.
		return false, nil
	}
	return false, err
}

// Rebase implements Git. --force-rebase writes new commits even when the
// branch is already on onto, so the push always starts fresh CI.
func (g *LocalGit) Rebase(ctx context.Context, onto, upstream, head string) (string, error) {
	if _, err := g.run(ctx, "rebase", "--quiet", "--force-rebase", "--no-autosquash", "--onto", onto, upstream, head); err != nil {
		if _, abortErr := g.run(ctx, "rebase", "--abort"); abortErr == nil {
			return "", ErrRebaseConflict
		}
		return "", err
	}
	return g.run(ctx, "rev-parse", "--verify", "HEAD^{commit}")
}

// PushBranches implements Git with one atomic push, each branch leased on
// its expected head.
func (g *LocalGit) PushBranches(ctx context.Context, updates []BranchUpdate) error {
	args := []string{"push", "--atomic", "--porcelain", "origin"}
	for _, update := range updates {
		args = append(args, "--force-with-lease=refs/heads/"+update.Branch+":"+update.Expected)
	}
	for _, update := range updates {
		args = append(args, update.SHA+":refs/heads/"+update.Branch)
	}
	output, err := g.run(ctx, args...)
	if err != nil && strings.Contains(output, "stale info") {
		return ErrLeaseRejected
	}
	return err
}

// FastForward implements Git with a push that is not forced.
func (g *LocalGit) FastForward(ctx context.Context, branch, sha string) error {
	output, err := g.run(ctx, "push", "--porcelain", "origin", sha+":refs/heads/"+branch)
	if err != nil && (strings.Contains(output, "non-fast-forward") || strings.Contains(output, "fetch first")) {
		return ErrNotFastForward
	}
	return err
}
