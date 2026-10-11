package mergequeue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRESTClientTrustsOnlyTheBotsStatusComment(t *testing.T) {
	gh := newFakeGitHub(t, "")
	ctx := context.Background()
	client := gh.client(appToken)
	gh.addComment(1, fakeUser{Login: botLogin, Type: "Bot"}, StatusPrefix+"\nMerge queue: mine")
	gh.addComment(1, fakeUser{Login: humanLogin, Type: "User"}, StatusPrefix+"\nMerge queue: forged")
	gh.addComment(1, fakeUser{Login: "other-app[bot]", Type: "Bot"}, StatusPrefix+"\nMerge queue: forged by another app")
	// A user account cannot be named like a bot, but the type check must
	// not depend on that.
	gh.addComment(1, fakeUser{Login: botLogin, Type: "User"}, StatusPrefix+"\nMerge queue: forged by a lookalike")

	body, err := client.StatusComment(ctx, 1, botLogin)
	if err != nil || body != StatusPrefix+"\nMerge queue: mine" {
		t.Fatalf("StatusComment = %q, %v; want the bot's own comment", body, err)
	}
	if body, _ := client.StatusComment(ctx, 1, ""); body != "" {
		t.Fatalf("StatusComment without a bot login = %q, want none", body)
	}
}

func TestRESTClientUpsertsOneStatusComment(t *testing.T) {
	gh := newFakeGitHub(t, "")
	ctx := context.Background()
	client := gh.client(appToken)
	for _, text := range []string{"one", "two", "two"} {
		if err := client.UpsertStatusComment(ctx, 7, botLogin, StatusPrefix+"\n"+text); err != nil {
			t.Fatal(err)
		}
	}
	if writes := strings.Join(gh.writeLog(), ","); writes != "POST /issues/7/comments,PATCH /issues/comments/1" {
		t.Fatalf("writes = %s, want one create, one edit, then nothing", writes)
	}
	if body := gh.status(7); body != StatusPrefix+"\ntwo" {
		t.Fatalf("status = %q", body)
	}
}

func TestRESTClientCanWrite(t *testing.T) {
	gh := newFakeGitHub(t, "")
	client := gh.client(appToken)
	for login, want := range map[string]bool{"alice": true, "root": true, "tracy": false, "stranger": false} {
		got, err := client.CanWrite(context.Background(), login)
		if err != nil || got != want {
			t.Errorf("CanWrite(%s) = %v, %v; want %v", login, got, err, want)
		}
	}
}

func TestRESTClientErrorsNeverCarryTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		http.Error(w, "echo "+r.Header.Get("Authorization"), http.StatusForbidden)
	}))
	defer server.Close()
	client := &RESTClient{Owner: "acme", Repo: "widgets", Token: "secret-token", BaseURL: server.URL}
	_, err := client.ResolveBranch(context.Background(), "main")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("ResolveBranch error = %v, want an error without the token", err)
	}
}

func TestRESTClientBoundsResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"sha":"` + strings.Repeat("a", maxGitHubResponse) + `"}}`))
	}))
	defer server.Close()
	client := &RESTClient{Owner: "acme", Repo: "widgets", Token: "t", BaseURL: server.URL}
	if _, err := client.ResolveBranch(context.Background(), "main"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("ResolveBranch error = %v, want a too-large error", err)
	}
}

func TestEscapePathKeepsSlashes(t *testing.T) {
	if got := escapePath("feature/a b#c"); got != "feature/a%20b%23c" {
		t.Fatalf("escapePath = %q", got)
	}
}
