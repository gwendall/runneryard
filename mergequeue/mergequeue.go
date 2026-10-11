// Package mergequeue is RunnerYard's merge queue (preview): a label-driven
// FIFO that lands pull requests on a target branch only after their
// required checks pass on the exact commit that will become the target.
//
// It keeps no state of its own. Each tick rebuilds everything from GitHub
// (the open pull requests carrying the queue label, the queue's own status
// comments, check runs), plans with a pure function (PlanTick), then
// applies the plan. A tick runs inside a GitHub Actions workflow, never
// inside the runner controller, with a dedicated GitHub App installation
// token. See docs/merge-queue.md.
package mergequeue

import (
	"context"
	"errors"
	"time"
)

// DefaultLabel is the label that puts a pull request in the queue.
const DefaultLabel = "queue"

// ConfigPath is where the queue reads its configuration, on the default
// branch, never from a pull request.
const ConfigPath = ".github/merge-queue.yml"

// The workflow reads the queue App's credentials from these repository
// settings: the client ID as an Actions variable, the key as a secret.
const (
	AppClientIDVariable = "MERGE_QUEUE_APP_CLIENT_ID"
	AppPrivateKeySecret = "MERGE_QUEUE_APP_PRIVATE_KEY" // #nosec G101 -- the secret's name, not a credential
)

// AppPermissions are the repository permissions of the queue's GitHub App,
// as docs/merge-queue.md lists them. workflows adds the one a queued pull
// request needs to change .github/workflows.
func AppPermissions(workflows bool) map[string]string {
	permissions := map[string]string{
		"actions":       "write", // find, cancel and re-run workflow runs
		"checks":        "read",  // read check runs on the top of a stack
		"contents":      "write", // read the configuration, push stacks, fast-forward the target
		"issues":        "write", // list labeled pull requests, edit the status comment, remove the label
		"metadata":      "read",  // collaborator permissions
		"pull_requests": "write", // read pull requests, comment on them
	}
	if workflows {
		permissions["workflows"] = "write"
	}
	return permissions
}

var (
	// ErrRebaseConflict means a pull request does not rebase cleanly onto
	// the stack below it. It is deterministic, so it is never retried.
	ErrRebaseConflict = errors.New("rebase conflict")
	// ErrLeaseRejected means a pull request branch moved after the
	// snapshot; the atomic push wrote nothing.
	ErrLeaseRejected = errors.New("a pull request branch moved since the snapshot")
	// ErrNotFastForward means the target branch moved since the snapshot;
	// the push wrote nothing.
	ErrNotFastForward = errors.New("the target branch moved since the snapshot")
)

// PR is an open pull request carrying the queue label, as GitHub reports it.
type PR struct {
	Number  int
	HeadSHA string
	// HeadRef is the head branch name. The queue only stacks same-repository
	// pull requests, so this is a branch of the repository itself.
	HeadRef  string
	BaseRef  string
	SameRepo bool
	Draft    bool
	// LabeledAt and LabeledBy describe the latest event that added the
	// queue label: FIFO order comes from the first, the permission check
	// from the second.
	LabeledAt time.Time
	LabeledBy string
}

// CheckRun is one GitHub check run on a commit.
type CheckRun struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	URL        string
	SuiteID    int64
	StartedAt  time.Time
}

// WorkflowRun is one GitHub Actions workflow run on a commit.
type WorkflowRun struct {
	ID         int64
	Event      string
	Status     string
	Conclusion string
	SuiteID    int64
}

// GitHub is everything a tick reads from or writes to the GitHub API.
// RESTClient implements it; tests use a fake server behind the same client.
type GitHub interface {
	ResolveBranch(ctx context.Context, branch string) (string, error)
	// ReadFile returns a file from the default branch.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	QueuedPRs(ctx context.Context, label string) ([]PR, error)
	// CanWrite reports whether login has write, maintain or admin access.
	CanWrite(ctx context.Context, login string) (bool, error)
	// StatusComment returns the body of the queue's status comment on a
	// pull request, only if botLogin wrote it, or "" when there is none.
	StatusComment(ctx context.Context, number int, botLogin string) (string, error)
	UpsertStatusComment(ctx context.Context, number int, botLogin, body string) error
	ChangedFiles(ctx context.Context, number int) ([]string, error)
	CheckRuns(ctx context.Context, sha string) ([]CheckRun, error)
	WorkflowRuns(ctx context.Context, sha string) ([]WorkflowRun, error)
	// IsAncestor reports whether ancestor is reachable from descendant.
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
	CancelRun(ctx context.Context, id int64) error
	RerunFailedJobs(ctx context.Context, id int64) error
	RemoveLabel(ctx context.Context, number int, label string) error
}

// BranchUpdate moves one pull request branch from Expected to SHA.
type BranchUpdate struct {
	Branch, SHA, Expected string
}

// Git is the local clone a tick builds stacks in. LocalGit implements it.
type Git interface {
	// Fetch fetches ref from the remote and returns the SHA it points to.
	Fetch(ctx context.Context, ref string) (string, error)
	// IsAncestor reports whether ancestor is reachable from descendant;
	// an unknown commit counts as not reachable.
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
	// Rebase replays upstream..head onto onto, always writing new commits,
	// and returns the new head. It returns ErrRebaseConflict on conflict.
	Rebase(ctx context.Context, onto, upstream, head string) (string, error)
	// PushBranches moves every branch atomically, each one only from its
	// Expected SHA. It returns ErrLeaseRejected when a branch moved.
	PushBranches(ctx context.Context, updates []BranchUpdate) error
	// FastForward moves branch to sha without force. It returns
	// ErrNotFastForward when branch is no longer an ancestor of sha.
	FastForward(ctx context.Context, branch, sha string) error
}
