package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	jwt "github.com/golang-jwt/jwt/v4"
)

func trustServer(t *testing.T, handler http.HandlerFunc) API {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return API{BaseURL: server.URL, Client: server.Client()}
}

func respondJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestCheckPrivateTargetRepositoryVisibility(t *testing.T) {
	for name, test := range map[string]struct {
		status     int
		body       string
		wantErr    bool
		wantPublic bool
	}{
		"private":                    {status: http.StatusOK, body: `{"private":true,"visibility":"private"}`},
		"internal":                   {status: http.StatusOK, body: `{"private":true,"visibility":"internal"}`},
		"public":                     {status: http.StatusOK, body: `{"private":false,"visibility":"public"}`, wantErr: true, wantPublic: true},
		"public without visibility":  {status: http.StatusOK, body: `{"private":false}`, wantErr: true, wantPublic: true},
		"private without visibility": {status: http.StatusOK, body: `{"private":true}`, wantErr: true, wantPublic: true},
		"contradictory answer":       {status: http.StatusOK, body: `{"private":true,"visibility":"public"}`, wantErr: true, wantPublic: true},
		"not found":                  {status: http.StatusNotFound, body: `{"message":"Not Found"}`, wantErr: true},
		"server error":               {status: http.StatusInternalServerError, body: `{}`, wantErr: true},
		"undecodable":                {status: http.StatusOK, body: `{"private":`, wantErr: true},
		"private as a string":        {status: http.StatusOK, body: `{"private":"true","visibility":"private"}`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			api := trustServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/app" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer installation-token" {
					t.Errorf("authorization = %q", r.Header.Get("Authorization"))
				}
				respondJSON(w, test.status, test.body)
			})
			err := api.CheckPrivateTarget(context.Background(), "installation-token", Target{Owner: "acme", Repository: "app"}, "ignored")
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, want error %v", err, test.wantErr)
			}
			if errors.Is(err, ErrPublicTarget) != test.wantPublic {
				t.Fatalf("errors.Is(%v, ErrPublicTarget) = %v, want %v", err, !test.wantPublic, test.wantPublic)
			}
			if requests != 1 {
				t.Fatalf("requests = %d", requests)
			}
		})
	}
}

func TestCheckPrivateTargetOrganizationRunnerGroup(t *testing.T) {
	for name, test := range map[string]struct {
		group      string
		status     int
		body       string
		wantErr    bool
		wantPublic bool
	}{
		"refuses public repositories": {group: "ci", status: http.StatusOK, body: `{"runner_groups":[{"id":1,"name":"Default","allows_public_repositories":true},{"id":2,"name":"ci","allows_public_repositories":false}]}`},
		"name matches any case":       {group: "CI", status: http.StatusOK, body: `{"runner_groups":[{"id":2,"name":"ci","allows_public_repositories":false}]}`},
		"empty group is Default":      {group: "", status: http.StatusOK, body: `{"runner_groups":[{"id":1,"name":"Default","allows_public_repositories":false}]}`},
		"allows public repositories":  {group: "ci", status: http.StatusOK, body: `{"runner_groups":[{"id":2,"name":"ci","allows_public_repositories":true}]}`, wantErr: true, wantPublic: true},
		"field missing":               {group: "ci", status: http.StatusOK, body: `{"runner_groups":[{"id":2,"name":"ci"}]}`, wantErr: true, wantPublic: true},
		"field null":                  {group: "ci", status: http.StatusOK, body: `{"runner_groups":[{"id":2,"name":"ci","allows_public_repositories":null}]}`, wantErr: true, wantPublic: true},
		"group absent":                {group: "ci", status: http.StatusOK, body: `{"runner_groups":[{"id":1,"name":"Default","allows_public_repositories":false}]}`, wantErr: true},
		"no groups":                   {group: "ci", status: http.StatusOK, body: `{"total_count":0,"runner_groups":[]}`, wantErr: true},
		"forbidden":                   {group: "ci", status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`, wantErr: true},
		"server error":                {group: "ci", status: http.StatusBadGateway, body: `{}`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			api := trustServer(t, func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/orgs/acme/actions/runner-groups" || query.Get("per_page") != "100" || query.Get("page") != "1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
				}
				if r.Header.Get("Authorization") != "Bearer installation-token" {
					t.Errorf("authorization = %q", r.Header.Get("Authorization"))
				}
				respondJSON(w, test.status, test.body)
			})
			err := api.CheckPrivateTarget(context.Background(), "installation-token", Target{Owner: "acme"}, test.group)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, want error %v", err, test.wantErr)
			}
			if errors.Is(err, ErrPublicTarget) != test.wantPublic {
				t.Fatalf("errors.Is(%v, ErrPublicTarget) = %v, want %v", err, !test.wantPublic, test.wantPublic)
			}
		})
	}
}

func runnerGroupsPage(count int, extra ...string) string {
	groups := make([]string, 0, count+len(extra))
	for i := range count {
		groups = append(groups, fmt.Sprintf(`{"id":%d,"name":"other-%d","allows_public_repositories":true}`, 100+i, i))
	}
	groups = append(groups, extra...)
	return `{"runner_groups":[` + strings.Join(groups, ",") + `]}`
}

func TestCheckPrivateTargetPaginatesRunnerGroups(t *testing.T) {
	ci := `{"id":7,"name":"ci","allows_public_repositories":false}`
	for name, test := range map[string]struct {
		pages     func(page int) string
		wantPages []string
		wantErr   bool
	}{
		"found on the second page": {
			pages: func(page int) string {
				if page == 1 {
					return runnerGroupsPage(100)
				}
				return runnerGroupsPage(0, ci)
			},
			wantPages: []string{"1", "2"},
		},
		"short page ends the search": {
			pages: func(page int) string {
				if page == 1 {
					return runnerGroupsPage(100)
				}
				return runnerGroupsPage(3)
			},
			wantPages: []string{"1", "2"},
			wantErr:   true,
		},
		"bounded when every page is full": {
			pages:     func(int) string { return runnerGroupsPage(100) },
			wantPages: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"},
			wantErr:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var pages []string
			api := trustServer(t, func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				mu.Lock()
				pages = append(pages, page)
				mu.Unlock()
				number := 0
				_, _ = fmt.Sscanf(page, "%d", &number)
				respondJSON(w, http.StatusOK, test.pages(number))
			})
			err := api.CheckPrivateTarget(context.Background(), "installation-token", Target{Owner: "acme"}, "ci")
			if (err != nil) != test.wantErr || errors.Is(err, ErrPublicTarget) {
				t.Fatalf("err = %v, want error %v and never ErrPublicTarget", err, test.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(pages, test.wantPages) {
				t.Fatalf("pages = %v, want %v", pages, test.wantPages)
			}
		})
	}
}

func TestCheckPrivateTargetRejectsBadInputWithoutCalls(t *testing.T) {
	api := trustServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		respondJSON(w, http.StatusOK, `{"private":true,"visibility":"private"}`)
	})
	for name, test := range map[string]struct {
		token  string
		target Target
	}{
		"no token":           {target: Target{Owner: "acme", Repository: "app"}},
		"no owner":           {token: "installation-token", target: Target{Repository: "app"}},
		"owner with a slash": {token: "installation-token", target: Target{Owner: "acme/other", Repository: "app"}},
		"repository path":    {token: "installation-token", target: Target{Owner: "acme", Repository: "app/../../orgs"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := api.CheckPrivateTarget(context.Background(), test.token, test.target, "")
			if err == nil || errors.Is(err, ErrPublicTarget) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestInstallationTokenUsesTheAppJWT(t *testing.T) {
	privateKey := testPrivateKey(t)
	signingKey, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKey))
	if err != nil {
		t.Fatal(err)
	}
	credentials := Credentials{ClientID: "Iv1.test", InstallationID: 4242, PrivateKey: privateKey}
	requests := 0
	api := trustServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/4242/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		claims := &jwt.RegisteredClaims{}
		token, err := jwt.ParseWithClaims(bearer, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodRS256 {
				return nil, fmt.Errorf("signing method %v", token.Header["alg"])
			}
			return &signingKey.PublicKey, nil
		})
		if err != nil || !token.Valid || claims.Issuer != "Iv1.test" {
			t.Errorf("app JWT is not valid for the App: %v, %#v", err, claims)
		}
		respondJSON(w, http.StatusCreated, `{"token":"ghs_installation","expires_at":"2026-10-11T10:00:00Z"}`)
	})
	token, err := api.InstallationToken(context.Background(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	if token != "ghs_installation" || requests != 1 {
		t.Fatalf("token = %q after %d requests", token, requests)
	}
}

func TestInstallationTokenFailures(t *testing.T) {
	privateKey := testPrivateKey(t)
	for name, test := range map[string]struct {
		credentials Credentials
		status      int
		body        string
		wantCalls   int
	}{
		"incomplete credentials": {credentials: Credentials{ClientID: "Iv1.test", PrivateKey: privateKey}},
		"invalid private key":    {credentials: Credentials{ClientID: "Iv1.test", InstallationID: 4242, PrivateKey: "not a key"}},
		"wrong status":           {credentials: Credentials{ClientID: "Iv1.test", InstallationID: 4242, PrivateKey: privateKey}, status: http.StatusOK, body: `{"token":"ghs_installation"}`, wantCalls: 1},
		"refused":                {credentials: Credentials{ClientID: "Iv1.test", InstallationID: 4242, PrivateKey: privateKey}, status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`, wantCalls: 1},
		"empty token":            {credentials: Credentials{ClientID: "Iv1.test", InstallationID: 4242, PrivateKey: privateKey}, status: http.StatusCreated, body: `{"token":""}`, wantCalls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			api := trustServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				respondJSON(w, test.status, test.body)
			})
			token, err := api.InstallationToken(context.Background(), test.credentials)
			if err == nil || token != "" {
				t.Fatalf("token = %q, err = %v", token, err)
			}
			if calls != test.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, test.wantCalls)
			}
		})
	}
}
