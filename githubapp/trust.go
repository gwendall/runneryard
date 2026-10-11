package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrPublicTarget reports a definite answer: the target can run public code.
var ErrPublicTarget = errors.New("the GitHub target can run public repositories")

// InstallationToken returns a short-lived installation access token for the
// App's own reads, such as the private-target check.
func (api API) InstallationToken(ctx context.Context, credentials Credentials) (string, error) {
	api = api.defaults()
	if err := credentials.Validate(); err != nil {
		return "", err
	}
	appToken, err := api.appJWT(credentials)
	if err != nil {
		return "", err
	}
	var issued struct {
		Token string `json:"token"`
	}
	path := "/app/installations/" + strconv.FormatInt(credentials.InstallationID, 10) + "/access_tokens"
	if err := api.doJSON(ctx, http.MethodPost, path, appToken, nil, &issued, http.StatusCreated); err != nil {
		return "", fmt.Errorf("create GitHub App installation token: %w", err)
	}
	if issued.Token == "" {
		return "", errors.New("GitHub returned an empty installation token")
	}
	return issued.Token, nil
}

// CheckPrivateTarget verifies that the runner scale set can only serve
// private code: a repository target must not be public, and an organization
// target's runner group must refuse public repositories. Shared pool hosts
// rely on it, since one job's privileged container can reach the next job's.
// Any answer other than a clear "private" fails closed.
func (api API) CheckPrivateTarget(ctx context.Context, token string, target Target, runnerGroup string) error {
	api = api.defaults()
	if token == "" {
		return errors.New("checking that the GitHub target is private requires a token")
	}
	if target.Owner == "" || !safeOwner.MatchString(target.Owner) || (target.Repository != "" && !safeRepo.MatchString(target.Repository)) {
		return errors.New("GitHub target contains an invalid owner or repository name")
	}
	if target.Repository != "" {
		var repository struct {
			Private    bool   `json:"private"`
			Visibility string `json:"visibility"`
		}
		path := "/repos/" + url.PathEscape(target.Owner) + "/" + url.PathEscape(target.Repository)
		if err := api.doJSON(ctx, http.MethodGet, path, token, nil, &repository, http.StatusOK); err != nil {
			return fmt.Errorf("read visibility of %s/%s: %w", target.Owner, target.Repository, err)
		}
		if !repository.Private || (repository.Visibility != "private" && repository.Visibility != "internal") {
			return fmt.Errorf("%w: %s/%s is %s; pool hosts only serve private repositories", ErrPublicTarget, target.Owner, target.Repository, firstNonEmpty(repository.Visibility, "public"))
		}
		return nil
	}
	if runnerGroup == "" {
		runnerGroup = "Default"
	}
	for page := 1; page <= 10; page++ {
		var groups struct {
			TotalCount   int `json:"total_count"`
			RunnerGroups []struct {
				ID                       int64  `json:"id"`
				Name                     string `json:"name"`
				AllowsPublicRepositories *bool  `json:"allows_public_repositories"`
			} `json:"runner_groups"`
		}
		path := "/orgs/" + url.PathEscape(target.Owner) + "/actions/runner-groups?per_page=100&page=" + strconv.Itoa(page)
		if err := api.doJSON(ctx, http.MethodGet, path, token, nil, &groups, http.StatusOK); err != nil {
			return fmt.Errorf("read runner groups of %s: %w", target.Owner, err)
		}
		for _, group := range groups.RunnerGroups {
			if !strings.EqualFold(group.Name, runnerGroup) {
				continue
			}
			if group.AllowsPublicRepositories == nil || *group.AllowsPublicRepositories {
				return fmt.Errorf("%w: runner group %q of %s allows public repositories; turn that off before using pool hosts", ErrPublicTarget, group.Name, target.Owner)
			}
			return nil
		}
		if len(groups.RunnerGroups) < 100 {
			break
		}
	}
	return fmt.Errorf("runner group %q of %s was not found", runnerGroup, target.Owner)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
