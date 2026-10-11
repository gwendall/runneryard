package mergequeue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGitHubAPIURL = "https://api.github.com"
	maxGitHubResponse   = 1 << 20
	// maxCompareResponse bounds a compare response, which embeds commits
	// and files.
	maxCompareResponse = 8 << 20
	pageSize           = 100
	// maxPages bounds every paginated listing; GitHub itself stops listing
	// pull request files at 3000.
	maxPages = 30
)

// RESTClient implements GitHub against the GitHub REST API with a token
// the workflow provides: the queue App's installation token, or the
// workflow's own read-only token for a dry run. It never logs the token.
type RESTClient struct {
	Owner, Repo string
	Token       string
	// BaseURL defaults to https://api.github.com; tests point it at a fake.
	BaseURL string
	Client  *http.Client
}

func (c *RESTClient) repoPath(suffix string) string {
	return "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo) + suffix
}

// escapePath escapes each segment of a slash-separated name, such as a
// branch or a file path.
func escapePath(name string) string {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// ResolveBranch implements GitHub.
func (c *RESTClient) ResolveBranch(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.doJSON(ctx, http.MethodGet, c.repoPath("/git/ref/heads/"+escapePath(branch)), nil, &ref, http.StatusOK); err != nil {
		return "", fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	return ref.Object.SHA, nil
}

// ReadFile implements GitHub.
func (c *RESTClient) ReadFile(ctx context.Context, path string) ([]byte, error) {
	body, _, err := c.do(ctx, http.MethodGet, c.repoPath("/contents/"+escapePath(path)), nil, "application/vnd.github.raw+json", maxGitHubResponse, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return body, nil
}

// QueuedPRs implements GitHub.
func (c *RESTClient) QueuedPRs(ctx context.Context, label string) ([]PR, error) {
	issues, err := getList[struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	}](ctx, c, c.repoPath("/issues?state=open&labels="+url.QueryEscape(label)))
	if err != nil {
		return nil, fmt.Errorf("list queued pull requests: %w", err)
	}
	var prs []PR
	for _, issue := range issues {
		if len(issue.PullRequest) == 0 || string(issue.PullRequest) == "null" {
			continue
		}
		pr, err := c.pullRequest(ctx, issue.Number)
		if err != nil {
			return nil, err
		}
		if pr.LabeledAt, pr.LabeledBy, err = c.lastLabeled(ctx, issue.Number, label); err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

type repository struct {
	FullName string `json:"full_name"`
}

func (c *RESTClient) pullRequest(ctx context.Context, number int) (PR, error) {
	var pull struct {
		Draft bool `json:"draft"`
		Head  struct {
			SHA  string      `json:"sha"`
			Ref  string      `json:"ref"`
			Repo *repository `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string      `json:"ref"`
			Repo *repository `json:"repo"`
		} `json:"base"`
	}
	if err := c.doJSON(ctx, http.MethodGet, c.repoPath("/pulls/"+strconv.Itoa(number)), nil, &pull, http.StatusOK); err != nil {
		return PR{}, fmt.Errorf("pull request #%d: %w", number, err)
	}
	sameRepo := pull.Head.Repo != nil && pull.Base.Repo != nil && strings.EqualFold(pull.Head.Repo.FullName, pull.Base.Repo.FullName)
	return PR{
		Number:   number,
		HeadSHA:  pull.Head.SHA,
		HeadRef:  pull.Head.Ref,
		BaseRef:  pull.Base.Ref,
		SameRepo: sameRepo,
		Draft:    pull.Draft,
	}, nil
}

func (c *RESTClient) lastLabeled(ctx context.Context, number int, label string) (time.Time, string, error) {
	events, err := getList[struct {
		Event string `json:"event"`
		Label struct {
			Name string `json:"name"`
		} `json:"label"`
		Actor struct {
			Login string `json:"login"`
		} `json:"actor"`
		CreatedAt time.Time `json:"created_at"`
	}](ctx, c, c.repoPath("/issues/"+strconv.Itoa(number)+"/timeline"))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("timeline of #%d: %w", number, err)
	}
	var at time.Time
	var by string
	for _, event := range events {
		if event.Event == "labeled" && strings.EqualFold(event.Label.Name, label) && !event.CreatedAt.Before(at) {
			at, by = event.CreatedAt, event.Actor.Login
		}
	}
	return at, by, nil
}

// CanWrite implements GitHub. The legacy permission field reports maintain
// as write and triage as read.
func (c *RESTClient) CanWrite(ctx context.Context, login string) (bool, error) {
	var permission struct {
		Permission string `json:"permission"`
	}
	err := c.doJSON(ctx, http.MethodGet, c.repoPath("/collaborators/"+url.PathEscape(login)+"/permission"), nil, &permission, http.StatusOK)
	var status *githubAPIStatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("permission of %s: %w", login, err)
	}
	return permission.Permission == "admin" || permission.Permission == "write", nil
}

type issueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

// statusComment finds the newest status comment botLogin wrote. Comments by
// anyone else are ignored, so a copied marker is never trusted.
func (c *RESTClient) statusComment(ctx context.Context, number int, botLogin string) (issueComment, bool, error) {
	comments, err := getList[issueComment](ctx, c, c.repoPath("/issues/"+strconv.Itoa(number)+"/comments"))
	if err != nil {
		return issueComment{}, false, fmt.Errorf("comments of #%d: %w", number, err)
	}
	for _, comment := range slices.Backward(comments) {
		if comment.User.Type == "Bot" && strings.EqualFold(comment.User.Login, botLogin) && strings.HasPrefix(comment.Body, StatusPrefix) {
			return comment, true, nil
		}
	}
	return issueComment{}, false, nil
}

// StatusComment implements GitHub.
func (c *RESTClient) StatusComment(ctx context.Context, number int, botLogin string) (string, error) {
	comment, _, err := c.statusComment(ctx, number, botLogin)
	return comment.Body, err
}

// UpsertStatusComment implements GitHub: it edits the existing status
// comment, so a long queue does not flood the timeline, and skips the edit
// when nothing changed.
func (c *RESTClient) UpsertStatusComment(ctx context.Context, number int, botLogin, body string) error {
	comment, found, err := c.statusComment(ctx, number, botLogin)
	if err != nil {
		return err
	}
	if found && comment.Body == body {
		return nil
	}
	if found {
		path := c.repoPath("/issues/comments/" + strconv.FormatInt(comment.ID, 10))
		if err := c.doJSON(ctx, http.MethodPatch, path, map[string]any{"body": body}, nil, http.StatusOK); err != nil {
			return fmt.Errorf("edit the status comment of #%d: %w", number, err)
		}
		return nil
	}
	path := c.repoPath("/issues/" + strconv.Itoa(number) + "/comments")
	if err := c.doJSON(ctx, http.MethodPost, path, map[string]any{"body": body}, nil, http.StatusCreated); err != nil {
		return fmt.Errorf("comment on #%d: %w", number, err)
	}
	return nil
}

// ChangedFiles implements GitHub.
func (c *RESTClient) ChangedFiles(ctx context.Context, number int) ([]string, error) {
	files, err := getList[struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
	}](ctx, c, c.repoPath("/pulls/"+strconv.Itoa(number)+"/files"))
	if err != nil {
		return nil, fmt.Errorf("files of #%d: %w", number, err)
	}
	var names []string
	for _, file := range files {
		names = append(names, file.Filename)
		if file.PreviousFilename != "" {
			names = append(names, file.PreviousFilename)
		}
	}
	return names, nil
}

// CheckRuns implements GitHub. It lists every run, not only the latest per
// name, and Evaluate keeps the newest.
func (c *RESTClient) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var runs []CheckRun
	for page := 1; page <= maxPages; page++ {
		var body struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				ID         int64     `json:"id"`
				Name       string    `json:"name"`
				Status     string    `json:"status"`
				Conclusion string    `json:"conclusion"`
				HTMLURL    string    `json:"html_url"`
				StartedAt  time.Time `json:"started_at"`
				CheckSuite struct {
					ID int64 `json:"id"`
				} `json:"check_suite"`
			} `json:"check_runs"`
		}
		path := c.repoPath("/commits/" + url.PathEscape(sha) + "/check-runs?filter=all&per_page=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page))
		if err := c.doJSON(ctx, http.MethodGet, path, nil, &body, http.StatusOK); err != nil {
			return nil, fmt.Errorf("check runs of %s: %w", shortSHA(sha), err)
		}
		for _, run := range body.CheckRuns {
			runs = append(runs, CheckRun{
				ID: run.ID, Name: run.Name, Status: run.Status, Conclusion: run.Conclusion,
				URL: run.HTMLURL, SuiteID: run.CheckSuite.ID, StartedAt: run.StartedAt,
			})
		}
		if len(body.CheckRuns) < pageSize || len(runs) >= body.TotalCount {
			break
		}
	}
	return runs, nil
}

// WorkflowRuns implements GitHub.
func (c *RESTClient) WorkflowRuns(ctx context.Context, sha string) ([]WorkflowRun, error) {
	var body struct {
		WorkflowRuns []struct {
			ID           int64  `json:"id"`
			Event        string `json:"event"`
			Status       string `json:"status"`
			Conclusion   string `json:"conclusion"`
			CheckSuiteID int64  `json:"check_suite_id"`
		} `json:"workflow_runs"`
	}
	path := c.repoPath("/actions/runs?head_sha=" + url.QueryEscape(sha) + "&per_page=" + strconv.Itoa(pageSize))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &body, http.StatusOK); err != nil {
		return nil, fmt.Errorf("workflow runs of %s: %w", shortSHA(sha), err)
	}
	runs := make([]WorkflowRun, 0, len(body.WorkflowRuns))
	for _, run := range body.WorkflowRuns {
		runs = append(runs, WorkflowRun{ID: run.ID, Event: run.Event, Status: run.Status, Conclusion: run.Conclusion, SuiteID: run.CheckSuiteID})
	}
	return runs, nil
}

// IsAncestor implements GitHub with the compare API: base...head is "ahead"
// or "identical" exactly when base is reachable from head.
func (c *RESTClient) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	var compare struct {
		Status string `json:"status"`
	}
	path := c.repoPath("/compare/" + url.PathEscape(ancestor) + "..." + url.PathEscape(descendant) + "?per_page=1")
	body, _, err := c.do(ctx, http.MethodGet, path, nil, "application/vnd.github+json", maxCompareResponse, http.StatusOK)
	var status *githubAPIStatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare %s with %s: %w", shortSHA(ancestor), shortSHA(descendant), err)
	}
	if err := json.Unmarshal(body, &compare); err != nil {
		return false, errors.New("decode GitHub compare response")
	}
	return compare.Status == "ahead" || compare.Status == "identical", nil
}

// CancelRun implements GitHub. A run that already finished is not an error.
func (c *RESTClient) CancelRun(ctx context.Context, id int64) error {
	_, _, err := c.do(ctx, http.MethodPost, c.repoPath("/actions/runs/"+strconv.FormatInt(id, 10)+"/cancel"), nil, "application/vnd.github+json", maxGitHubResponse, http.StatusAccepted, http.StatusConflict)
	if err != nil {
		return fmt.Errorf("cancel run %d: %w", id, err)
	}
	return nil
}

// RerunFailedJobs implements GitHub.
func (c *RESTClient) RerunFailedJobs(ctx context.Context, id int64) error {
	if err := c.doJSON(ctx, http.MethodPost, c.repoPath("/actions/runs/"+strconv.FormatInt(id, 10)+"/rerun-failed-jobs"), nil, nil, http.StatusCreated); err != nil {
		return fmt.Errorf("re-run the failed jobs of run %d: %w", id, err)
	}
	return nil
}

// RemoveLabel implements GitHub. A label already gone is not an error.
func (c *RESTClient) RemoveLabel(ctx context.Context, number int, label string) error {
	path := c.repoPath("/issues/" + strconv.Itoa(number) + "/labels/" + url.PathEscape(label))
	if _, _, err := c.do(ctx, http.MethodDelete, path, nil, "application/vnd.github+json", maxGitHubResponse, http.StatusOK, http.StatusNotFound); err != nil {
		return fmt.Errorf("remove the %s label from #%d: %w", label, number, err)
	}
	return nil
}

// getList reads a paginated JSON array, maxPages pages at most.
func getList[T any](ctx context.Context, c *RESTClient, path string) ([]T, error) {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	var all []T
	for page := 1; page <= maxPages; page++ {
		var items []T
		paged := path + separator + "per_page=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page)
		if err := c.doJSON(ctx, http.MethodGet, paged, nil, &items, http.StatusOK); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize {
			break
		}
	}
	return all, nil
}

func (c *RESTClient) doJSON(ctx context.Context, method, path string, input, output any, expected int) error {
	body, _, err := c.do(ctx, method, path, input, "application/vnd.github+json", maxGitHubResponse, expected)
	if err != nil || output == nil {
		return err
	}
	if err := json.Unmarshal(body, output); err != nil {
		return errors.New("decode GitHub response")
	}
	return nil
}

func (c *RESTClient) do(ctx context.Context, method, path string, input any, accept string, limit int64, expected ...int) ([]byte, int, error) {
	var body io.Reader = http.NoBody
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, 0, errors.New("encode GitHub request")
		}
		body = bytes.NewReader(encoded)
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultGitHubAPIURL
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, 0, errors.New("create GitHub request")
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, response.StatusCode, errors.New("read GitHub response")
	}
	if !slices.Contains(expected, response.StatusCode) {
		return nil, response.StatusCode, &githubAPIStatusError{StatusCode: response.StatusCode, Status: response.Status}
	}
	if int64(len(data)) > limit {
		return nil, response.StatusCode, errors.New("GitHub response too large")
	}
	return data, response.StatusCode, nil
}

type githubAPIStatusError struct {
	StatusCode int
	Status     string
}

func (err *githubAPIStatusError) Error() string { return "GitHub returned " + err.Status }
