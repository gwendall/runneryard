package mergequeue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves the slice of the GitHub REST API the queue uses. Refs
// live in a real bare repository, so the queue's pushes and the API agree
// the way they do on GitHub, and a pull request counts as merged as soon as
// its head is on its base branch.
type fakeGitHub struct {
	t      *testing.T
	bare   string
	server *httptest.Server

	mu sync.Mutex
	// users maps a bearer token to the account it acts as.
	users       map[string]fakeUser
	config      string
	prs         map[int]*fakePR
	comments    []*fakeComment
	permissions map[string]string
	checkRuns   map[string][]fakeCheckRun
	runs        map[string][]fakeRun
	nextID      int64
	cancelled   []int64
	reruns      []int64
	writes      []string
	// mergeLag keeps landed pull requests open, as GitHub does for a moment
	// after the push.
	mergeLag bool
}

type fakeUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type fakePR struct {
	number int
	branch string
	base   string
	fork   bool
	draft  bool
	labels map[string]bool
	events []fakeEvent
	closed bool
}

type fakeEvent struct {
	Event     string            `json:"event"`
	Label     map[string]string `json:"label"`
	Actor     map[string]string `json:"actor"`
	CreatedAt time.Time         `json:"created_at"`
}

type fakeComment struct {
	ID     int64    `json:"id"`
	Body   string   `json:"body"`
	User   fakeUser `json:"user"`
	number int
}

type fakeCheckRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion *string   `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	StartedAt  time.Time `json:"started_at"`
	CheckSuite struct {
		ID int64 `json:"id"`
	} `json:"check_suite"`
}

type fakeRun struct {
	ID           int64   `json:"id"`
	Event        string  `json:"event"`
	Status       string  `json:"status"`
	Conclusion   *string `json:"conclusion"`
	CheckSuiteID int64   `json:"check_suite_id"`
}

const (
	fakeOwner    = "acme"
	fakeRepo     = "widgets"
	appToken     = "app-installation-token"
	botLogin     = "acme-merge-queue[bot]"
	humanToken   = "human-token"
	humanLogin   = "mallory"
	defaultCheck = "ci"
)

func newFakeGitHub(t *testing.T, bare string) *fakeGitHub {
	f := &fakeGitHub{
		t:    t,
		bare: bare,
		users: map[string]fakeUser{
			appToken:   {Login: botLogin, Type: "Bot"},
			humanToken: {Login: humanLogin, Type: "User"},
		},
		config:      "batch_size: 2\nrequired_checks:\n  - name: " + defaultCheck + "\n",
		prs:         map[int]*fakePR{},
		permissions: map[string]string{"alice": "write", "root": "admin", "tracy": "read"},
		checkRuns:   map[string][]fakeCheckRun{},
		runs:        map[string][]fakeRun{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) client(token string) *RESTClient {
	return &RESTClient{Owner: fakeOwner, Repo: fakeRepo, Token: token, BaseURL: f.server.URL, Client: f.server.Client()}
}

// git runs a read-only git command in the bare repository.
func (f *fakeGitHub) git(args ...string) (string, bool) {
	cmd := exec.Command("git", args...)
	cmd.Dir = f.bare
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err == nil
}

func (f *fakeGitHub) ref(branch string) string {
	sha, _ := f.git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return sha
}

func (f *fakeGitHub) isAncestor(ancestor, descendant string) bool {
	_, ok := f.git("merge-base", "--is-ancestor", ancestor, descendant)
	return ok
}

// open adds a pull request from branch to main and labels it as labeler.
func (f *fakeGitHub) open(number int, branch, labeler string, at time.Time) *fakePR {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := &fakePR{number: number, branch: branch, base: "main", labels: map[string]bool{}}
	f.prs[number] = pr
	f.labelLocked(pr, labeler, at)
	return pr
}

func (f *fakeGitHub) label(number int, labeler string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labelLocked(f.prs[number], labeler, at)
}

func (f *fakeGitHub) labelLocked(pr *fakePR, labeler string, at time.Time) {
	pr.labels["queue"] = true
	pr.events = append(pr.events, fakeEvent{Event: "labeled", Label: map[string]string{"name": "queue"}, Actor: map[string]string{"login": labeler}, CreatedAt: at})
}

func (f *fakeGitHub) unlabel(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.prs[number].labels, "queue")
}

// merged reports whether GitHub would show the pull request as merged.
func (f *fakeGitHub) merged(pr *fakePR) bool {
	if f.mergeLag {
		return false
	}
	head := f.ref(pr.branch)
	return head != "" && f.isAncestor(head, f.ref(pr.base))
}

func (f *fakeGitHub) isMerged(number int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.merged(f.prs[number])
}

func (f *fakeGitHub) hasLabel(number int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prs[number].labels["queue"]
}

// status returns the newest status comment the bot wrote on a pull request.
func (f *fakeGitHub) status(number int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := ""
	for _, comment := range f.comments {
		if comment.number == number && comment.User.Login == botLogin {
			body = comment.Body
		}
	}
	return body
}

func (f *fakeGitHub) addComment(number int, user fakeUser, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments = append(f.comments, &fakeComment{ID: f.nextID, Body: body, User: user, number: number})
}

// ci records one workflow run on sha reporting check name with conclusion
// ("" while it runs), started at startedAt.
func (f *fakeGitHub) ci(sha, name, conclusion string, startedAt time.Time) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	suite := f.nextID
	status := "completed"
	var result *string
	if conclusion == "" {
		status = "in_progress"
	} else {
		result = &conclusion
	}
	run := fakeCheckRun{ID: f.nextID, Name: name, Status: status, Conclusion: result, HTMLURL: "https://github.example/run/" + strconv.FormatInt(f.nextID, 10), StartedAt: startedAt}
	run.CheckSuite.ID = suite
	f.checkRuns[sha] = append(f.checkRuns[sha], run)
	f.nextID++
	f.runs[sha] = append(f.runs[sha], fakeRun{ID: f.nextID, Event: "pull_request", Status: status, Conclusion: result, CheckSuiteID: suite})
	return f.nextID
}

func (f *fakeGitHub) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

var (
	refPath         = regexp.MustCompile(`^/git/ref/heads/(.+)$`)
	pullPath        = regexp.MustCompile(`^/pulls/(\d+)$`)
	pullFilesPath   = regexp.MustCompile(`^/pulls/(\d+)/files$`)
	timelinePath    = regexp.MustCompile(`^/issues/(\d+)/timeline$`)
	commentsPath    = regexp.MustCompile(`^/issues/(\d+)/comments$`)
	commentPath     = regexp.MustCompile(`^/issues/comments/(\d+)$`)
	labelPath       = regexp.MustCompile(`^/issues/(\d+)/labels/(.+)$`)
	permissionPath  = regexp.MustCompile(`^/collaborators/([^/]+)/permission$`)
	checkRunsPath   = regexp.MustCompile(`^/commits/([0-9a-f]{40})/check-runs$`)
	comparePath     = regexp.MustCompile(`^/compare/([0-9a-f]{40})\.\.\.([0-9a-f]{40})$`)
	cancelPath      = regexp.MustCompile(`^/actions/runs/(\d+)/cancel$`)
	rerunPath       = regexp.MustCompile(`^/actions/runs/(\d+)/rerun-failed-jobs$`)
	maxCommentBytes = 65536
)

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, ok := f.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok || r.Header.Get("X-GitHub-Api-Version") == "" {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	prefix := "/repos/" + fakeOwner + "/" + fakeRepo
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	if r.Method != http.MethodGet {
		if user.Type != "Bot" {
			http.Error(w, "read-only token", http.StatusForbidden)
			return
		}
		f.writes = append(f.writes, r.Method+" "+path)
	}
	page := r.URL.Query().Get("page")
	if page != "" && page != "1" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	var match []string
	matches := func(pattern *regexp.Regexp) bool {
		match = pattern.FindStringSubmatch(path)
		return match != nil
	}
	number := func() *fakePR {
		n, _ := strconv.Atoi(match[1])
		return f.prs[n]
	}

	switch {
	case r.Method == http.MethodGet && matches(refPath):
		if sha := f.ref(match[1]); sha != "" {
			writeJSON(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": sha}})
			return
		}
	case r.Method == http.MethodGet && path == "/contents/.github/merge-queue.yml":
		if r.Header.Get("Accept") == "application/vnd.github.raw+json" {
			_, _ = w.Write([]byte(f.config))
			return
		}
	case r.Method == http.MethodGet && path == "/issues":
		label := r.URL.Query().Get("labels")
		var issues []map[string]any
		for n := 1; n <= 100; n++ {
			pr := f.prs[n]
			if pr == nil || pr.closed || f.merged(pr) || !pr.labels[label] {
				continue
			}
			issues = append(issues, map[string]any{"number": n, "pull_request": map[string]string{"url": "x"}})
		}
		writeJSON(w, http.StatusOK, issues)
		return
	case r.Method == http.MethodGet && matches(pullPath):
		if pr := number(); pr != nil {
			headRepo := fakeOwner + "/" + fakeRepo
			if pr.fork {
				headRepo = "someone/widgets"
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"draft": pr.draft,
				"head":  map[string]any{"sha": f.ref(pr.branch), "ref": pr.branch, "repo": map[string]string{"full_name": headRepo}},
				"base":  map[string]any{"ref": pr.base, "repo": map[string]string{"full_name": fakeOwner + "/" + fakeRepo}},
			})
			return
		}
	case r.Method == http.MethodGet && matches(pullFilesPath):
		if pr := number(); pr != nil {
			out, _ := f.git("diff", "--name-only", f.ref(pr.base)+"..."+f.ref(pr.branch))
			var files []map[string]string
			for _, name := range strings.Fields(out) {
				files = append(files, map[string]string{"filename": name})
			}
			writeJSON(w, http.StatusOK, files)
			return
		}
	case r.Method == http.MethodGet && matches(timelinePath):
		if pr := number(); pr != nil {
			writeJSON(w, http.StatusOK, pr.events)
			return
		}
	case r.Method == http.MethodGet && matches(permissionPath):
		if permission, ok := f.permissions[match[1]]; ok {
			writeJSON(w, http.StatusOK, map[string]string{"permission": permission})
			return
		}
	case r.Method == http.MethodGet && matches(commentsPath):
		n, _ := strconv.Atoi(match[1])
		var comments []*fakeComment
		for _, comment := range f.comments {
			if comment.number == n {
				comments = append(comments, comment)
			}
		}
		writeJSON(w, http.StatusOK, comments)
		return
	case r.Method == http.MethodPost && matches(commentsPath):
		var input struct{ Body string }
		if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Body) > maxCommentBytes {
			http.Error(w, "bad body", http.StatusUnprocessableEntity)
			return
		}
		n, _ := strconv.Atoi(match[1])
		f.nextID++
		f.comments = append(f.comments, &fakeComment{ID: f.nextID, Body: input.Body, User: user, number: n})
		writeJSON(w, http.StatusCreated, map[string]any{"id": f.nextID})
		return
	case r.Method == http.MethodPatch && matches(commentPath):
		id, _ := strconv.ParseInt(match[1], 10, 64)
		var input struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&input)
		for _, comment := range f.comments {
			if comment.ID == id && comment.User == user {
				comment.Body = input.Body
				writeJSON(w, http.StatusOK, map[string]any{"id": id})
				return
			}
		}
	case r.Method == http.MethodDelete && matches(labelPath):
		if pr := number(); pr != nil && pr.labels[match[2]] {
			delete(pr.labels, match[2])
			writeJSON(w, http.StatusOK, []any{})
			return
		}
	case r.Method == http.MethodGet && matches(checkRunsPath):
		runs := f.checkRuns[match[1]]
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
		return
	case r.Method == http.MethodGet && path == "/actions/runs":
		writeJSON(w, http.StatusOK, map[string]any{"workflow_runs": f.runs[r.URL.Query().Get("head_sha")]})
		return
	case r.Method == http.MethodGet && matches(comparePath):
		if _, ok := f.git("cat-file", "-e", match[1]+"^{commit}"); !ok {
			break
		}
		status := "diverged"
		if match[1] == match[2] {
			status = "identical"
		} else if f.isAncestor(match[1], match[2]) {
			status = "ahead"
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
		return
	case r.Method == http.MethodPost && matches(cancelPath):
		id, _ := strconv.ParseInt(match[1], 10, 64)
		for sha, runs := range f.runs {
			for i := range runs {
				if runs[i].ID == id && runs[i].Status != "completed" {
					cancelled := "cancelled"
					runs[i].Status, runs[i].Conclusion = "completed", &cancelled
					f.runs[sha] = runs
					f.cancelled = append(f.cancelled, id)
					writeJSON(w, http.StatusAccepted, map[string]any{})
					return
				}
			}
		}
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Cannot cancel a workflow run that is completed."})
		return
	case r.Method == http.MethodPost && matches(rerunPath):
		id, _ := strconv.ParseInt(match[1], 10, 64)
		f.reruns = append(f.reruns, id)
		writeJSON(w, http.StatusCreated, map[string]any{})
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
