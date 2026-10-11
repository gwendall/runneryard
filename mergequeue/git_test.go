package mergequeue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit seeds and inspects repositories without going through LocalGit.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newBareRepo creates a local bare repository standing in for GitHub, with
// one commit on main, and returns its path and that commit.
func newBareRepo(t *testing.T) (bare, mainSHA string) {
	t.Helper()
	dir := t.TempDir()
	bare = filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	runGit(t, dir, "init", "--quiet", "--bare", "-b", "main", bare)
	runGit(t, dir, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "--quiet", "-m", "seed")
	runGit(t, seed, "remote", "add", "origin", bare)
	runGit(t, seed, "push", "--quiet", "origin", "main")
	return bare, runGit(t, seed, "rev-parse", "main")
}

// pushBranch commits files on top of onto in a scratch clone of bare and
// pushes the result to branch.
func pushBranch(t *testing.T, bare, branch, onto string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	runGit(t, filepath.Dir(dir), "clone", "--quiet", bare, dir)
	runGit(t, dir, "checkout", "--quiet", "--detach", onto)
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", name)
	}
	runGit(t, dir, "commit", "--quiet", "-m", "change on "+branch)
	sha := runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "push", "--quiet", "--force", "origin", sha+":refs/heads/"+branch)
	return sha
}

func branchSHA(t *testing.T, bare, branch string) string {
	t.Helper()
	return runGit(t, bare, "rev-parse", "refs/heads/"+branch)
}

func cloneForTest(t *testing.T, bare string) *LocalGit {
	t.Helper()
	git, err := NewLocalGit(context.Background(), filepath.Join(t.TempDir(), "clone"), bare, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	return git
}

func TestLocalGitStacksAndPushesAtomically(t *testing.T) {
	ctx := context.Background()
	bare, mainSHA := newBareRepo(t)
	pr1 := pushBranch(t, bare, "feature-1", mainSHA, map[string]string{"one.txt": "1\n"})
	pr2 := pushBranch(t, bare, "feature-2", mainSHA, map[string]string{"two.txt": "2\n"})
	git := cloneForTest(t, bare)

	if sha, err := git.Fetch(ctx, "refs/heads/main"); err != nil || sha != mainSHA {
		t.Fatalf("Fetch(main) = %q, %v; want %q", sha, err, mainSHA)
	}
	if sha, err := git.Fetch(ctx, "refs/heads/feature-1"); err != nil || sha != pr1 {
		t.Fatalf("Fetch(feature-1) = %q, %v; want %q", sha, err, pr1)
	}
	if _, err := git.Fetch(ctx, "refs/heads/feature-2"); err != nil {
		t.Fatal(err)
	}
	tip1, err := git.Rebase(ctx, mainSHA, mainSHA, pr1)
	if err != nil {
		t.Fatal(err)
	}
	if tip1 == pr1 {
		t.Fatal("Rebase kept the old commit; a stack must always get fresh commits so its push starts CI")
	}
	tip2, err := git.Rebase(ctx, tip1, mainSHA, pr2)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := git.IsAncestor(ctx, tip1, tip2); err != nil || !ok {
		t.Fatalf("IsAncestor(tip1, tip2) = %v, %v; want true", ok, err)
	}
	if ok, err := git.IsAncestor(ctx, strings.Repeat("f", 40), tip2); err != nil || ok {
		t.Fatalf("IsAncestor(unknown, tip2) = %v, %v; want false, nil", ok, err)
	}
	if author := runGit(t, git.Dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>", tip2); author != "test <test@example.invalid>|runneryard-merge-queue <merge-queue@runneryard.invalid>" {
		t.Fatalf("author|committer = %q, want the author kept and the queue as committer", author)
	}

	if err := git.PushBranches(ctx, []BranchUpdate{
		{Branch: "feature-1", SHA: tip1, Expected: pr1},
		{Branch: "feature-2", SHA: tip2, Expected: pr2},
	}); err != nil {
		t.Fatal(err)
	}
	if got := branchSHA(t, bare, "feature-1"); got != tip1 {
		t.Fatalf("feature-1 = %s, want %s", got, tip1)
	}
	if err := git.FastForward(ctx, "main", tip2); err != nil {
		t.Fatal(err)
	}
	if got := branchSHA(t, bare, "main"); got != tip2 {
		t.Fatalf("main = %s, want %s", got, tip2)
	}
}

func TestLocalGitLeaseRejectionWritesNothing(t *testing.T) {
	ctx := context.Background()
	bare, mainSHA := newBareRepo(t)
	pr1 := pushBranch(t, bare, "feature-1", mainSHA, map[string]string{"one.txt": "1\n"})
	pr2 := pushBranch(t, bare, "feature-2", mainSHA, map[string]string{"two.txt": "2\n"})
	git := cloneForTest(t, bare)
	for _, branch := range []string{"feature-1", "feature-2"} {
		if _, err := git.Fetch(ctx, "refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
	}
	tip1, err := git.Rebase(ctx, mainSHA, mainSHA, pr1)
	if err != nil {
		t.Fatal(err)
	}
	tip2, err := git.Rebase(ctx, tip1, mainSHA, pr2)
	if err != nil {
		t.Fatal(err)
	}
	// The author pushes to feature-2 after the snapshot.
	moved := pushBranch(t, bare, "feature-2", pr2, map[string]string{"two.txt": "2 fixed\n"})

	err = git.PushBranches(ctx, []BranchUpdate{
		{Branch: "feature-1", SHA: tip1, Expected: pr1},
		{Branch: "feature-2", SHA: tip2, Expected: pr2},
	})
	if !errors.Is(err, ErrLeaseRejected) {
		t.Fatalf("PushBranches = %v, want ErrLeaseRejected", err)
	}
	if got := branchSHA(t, bare, "feature-1"); got != pr1 {
		t.Fatalf("feature-1 = %s, want it untouched at %s: the push must be atomic", got, pr1)
	}
	if got := branchSHA(t, bare, "feature-2"); got != moved {
		t.Fatalf("feature-2 = %s, want the author's %s", got, moved)
	}
}

func TestLocalGitFastForwardRefusesAMovedTarget(t *testing.T) {
	ctx := context.Background()
	bare, mainSHA := newBareRepo(t)
	pr1 := pushBranch(t, bare, "feature-1", mainSHA, map[string]string{"one.txt": "1\n"})
	git := cloneForTest(t, bare)
	if _, err := git.Fetch(ctx, "refs/heads/feature-1"); err != nil {
		t.Fatal(err)
	}
	moved := pushBranch(t, bare, "main", mainSHA, map[string]string{"hotfix.txt": "x\n"})
	if err := git.FastForward(ctx, "main", pr1); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("FastForward = %v, want ErrNotFastForward", err)
	}
	if got := branchSHA(t, bare, "main"); got != moved {
		t.Fatalf("main = %s, want it untouched at %s", got, moved)
	}
}

func TestLocalGitRebaseReportsConflict(t *testing.T) {
	ctx := context.Background()
	bare, mainSHA := newBareRepo(t)
	pr1 := pushBranch(t, bare, "feature-1", mainSHA, map[string]string{"shared.txt": "from pr1\n"})
	pr2 := pushBranch(t, bare, "feature-2", mainSHA, map[string]string{"shared.txt": "from pr2\n"})
	git := cloneForTest(t, bare)
	for _, branch := range []string{"feature-1", "feature-2"} {
		if _, err := git.Fetch(ctx, "refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
	}
	tip1, err := git.Rebase(ctx, mainSHA, mainSHA, pr1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := git.Rebase(ctx, tip1, mainSHA, pr2); !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("Rebase(pr2 onto pr1) = %v, want ErrRebaseConflict", err)
	}
	// The aborted rebase must leave the clone usable for the next stack.
	if _, err := git.Rebase(ctx, mainSHA, mainSHA, pr2); err != nil {
		t.Fatalf("Rebase after an aborted one: %v", err)
	}
}

func TestLocalGitRunsNoHooks(t *testing.T) {
	ctx := context.Background()
	bare, mainSHA := newBareRepo(t)
	pr1 := pushBranch(t, bare, "feature-1", mainSHA, map[string]string{"one.txt": "1\n"})
	git := cloneForTest(t, bare)
	if _, err := git.Fetch(ctx, "refs/heads/feature-1"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	for _, name := range []string{"post-checkout", "post-rewrite", "pre-push", "pre-commit"} {
		if err := os.WriteFile(filepath.Join(git.Dir, ".git", "hooks", name), []byte(hook), 0o700); err != nil { // #nosec G306 -- a test hook must be executable
			t.Fatal(err)
		}
	}
	tip, err := git.Rebase(ctx, mainSHA, mainSHA, pr1)
	if err != nil {
		t.Fatal(err)
	}
	if err := git.PushBranches(ctx, []BranchUpdate{{Branch: "feature-1", SHA: tip, Expected: pr1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository hook ran; the queue must never run code from the repository")
	}
}

func TestNewLocalGitKeepsTheTokenOutOfURLsAndConfig(t *testing.T) {
	bare, _ := newBareRepo(t)
	t.Setenv("MERGE_QUEUE_TOKEN", "secret-token")
	t.Setenv("GIT_DIR", "/elsewhere")
	git := cloneForTest(t, bare)
	for _, variable := range git.env {
		if strings.Contains(variable, "secret-token") || strings.HasPrefix(variable, "GIT_DIR=") {
			t.Fatalf("git inherits %q from the tick", variable)
		}
	}
	config, err := os.ReadFile(filepath.Join(git.Dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "extraheader") {
		t.Fatalf(".git/config holds an auth header:\n%s", config)
	}

	if _, err := NewLocalGit(context.Background(), t.TempDir(), "http://github.example/acme/widgets.git", "main", "secret-token"); err == nil {
		t.Fatal("NewLocalGit sent a token to a plain http remote")
	}
	// The clone fails (nothing listens there); what matters is how the token
	// was handed to git.
	remote := "https://127.0.0.1:1/acme/widgets.git"
	_, err = NewLocalGit(context.Background(), filepath.Join(t.TempDir(), "clone"), remote, "main", "secret-token")
	if err == nil {
		t.Fatal("expected the clone of an unreachable remote to fail")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("the error leaks the token: %v", err)
	}
}
